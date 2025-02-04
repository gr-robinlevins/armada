package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"

	lru "github.com/hashicorp/golang-lru"
	"github.com/oklog/ulid"
	"github.com/openconfig/goyang/pkg/indent"
	"github.com/pkg/errors"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"

	"github.com/armadaproject/armada/internal/common/armadaerrors"
	armadamaps "github.com/armadaproject/armada/internal/common/maps"
	schedulercontext "github.com/armadaproject/armada/internal/scheduler/context"
	"github.com/armadaproject/armada/internal/scheduler/schedulerobjects"
)

// SchedulingContextRepository stores scheduling contexts associated with recent scheduling attempts.
// On adding a context, a map is cloned, then mutated, and then swapped for the previous map using atomic pointers.
// Hence, reads concurrent with writes are safe and don't need locking.
// A mutex protects against concurrent writes.
type SchedulingContextRepository struct {
	// Maps executor id to *schedulercontext.SchedulingContext.
	// The most recent attempt.
	mostRecentSchedulingContextByExecutorP atomic.Pointer[SchedulingContextByExecutor]
	// The most recent attempt where a non-zero amount of resources were scheduled.
	mostRecentSuccessfulSchedulingContextByExecutorP atomic.Pointer[SchedulingContextByExecutor]
	// The most recent attempt that preempted at least one job.
	mostRecentPreemptingSchedulingContextByExecutorP atomic.Pointer[SchedulingContextByExecutor]

	// Maps queue name to QueueSchedulingContextByExecutor.
	// The most recent attempt.
	mostRecentQueueSchedulingContextByExecutorByQueueP atomic.Pointer[map[string]QueueSchedulingContextByExecutor]
	// The most recent attempt where a non-zero amount of resources were scheduled.
	mostRecentSuccessfulQueueSchedulingContextByExecutorByQueueP atomic.Pointer[map[string]QueueSchedulingContextByExecutor]
	// The most recent attempt that preempted at least one job belonging to this queue.
	mostRecentPreemptingQueueSchedulingContextByExecutorByQueueP atomic.Pointer[map[string]QueueSchedulingContextByExecutor]

	// Maps job id to JobSchedulingContextByExecutor.
	// We limit the number of job contexts to store to control memory usage.
	mostRecentJobSchedulingContextByExecutorByJobId *lru.Cache

	// Store all executor ids seen so far in a set.
	// Used to ensure all executors are included in reports.
	executorIds map[string]bool
	// All executors in sorted order.
	sortedExecutorIdsP atomic.Pointer[[]string]

	// Protects the fields in this struct from concurrent and dirty writes.
	mu sync.Mutex
}

type (
	SchedulingContextByExecutor      map[string]*schedulercontext.SchedulingContext
	QueueSchedulingContextByExecutor map[string]*schedulercontext.QueueSchedulingContext
	JobSchedulingContextByExecutor   map[string]*schedulercontext.JobSchedulingContext
)

func NewSchedulingContextRepository(maxJobSchedulingContextsPerExecutor uint) (*SchedulingContextRepository, error) {
	jobSchedulingContextByExecutorByJobId, err := lru.New(int(maxJobSchedulingContextsPerExecutor))
	if err != nil {
		return nil, err
	}
	rv := &SchedulingContextRepository{
		mostRecentJobSchedulingContextByExecutorByJobId: jobSchedulingContextByExecutorByJobId,
		executorIds: make(map[string]bool),
	}

	mostRecentSchedulingContextByExecutor := make(SchedulingContextByExecutor)
	mostRecentSuccessfulSchedulingContextByExecutor := make(SchedulingContextByExecutor)
	mostRecentPreemptingSchedulingContextByExecutorP := make(SchedulingContextByExecutor)

	mostRecentQueueSchedulingContextByExecutorByQueue := make(map[string]QueueSchedulingContextByExecutor)
	mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue := make(map[string]QueueSchedulingContextByExecutor)
	mostRecentPreemptingQueueSchedulingContextByExecutorByQueue := make(map[string]QueueSchedulingContextByExecutor)

	sortedExecutorIds := make([]string, 0)

	rv.mostRecentSchedulingContextByExecutorP.Store(&mostRecentSchedulingContextByExecutor)
	rv.mostRecentSuccessfulSchedulingContextByExecutorP.Store(&mostRecentSuccessfulSchedulingContextByExecutor)
	rv.mostRecentPreemptingSchedulingContextByExecutorP.Store(&mostRecentPreemptingSchedulingContextByExecutorP)

	rv.mostRecentQueueSchedulingContextByExecutorByQueueP.Store(&mostRecentQueueSchedulingContextByExecutorByQueue)
	rv.mostRecentSuccessfulQueueSchedulingContextByExecutorByQueueP.Store(&mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue)
	rv.mostRecentPreemptingQueueSchedulingContextByExecutorByQueueP.Store(&mostRecentPreemptingQueueSchedulingContextByExecutorByQueue)

	rv.sortedExecutorIdsP.Store(&sortedExecutorIds)

	return rv, nil
}

// AddSchedulingContext adds a scheduling context to the repo.
// It also extracts the queue and job scheduling contexts it contains and stores those separately.
//
// It's safe to call this method concurrently with itself and with methods getting contexts from the repo.
// It's not safe to mutate contexts once they've been provided to this method.
//
// Job contexts are stored first, then queue contexts, and finally the scheduling context itself.
// This avoids having a stored scheduling (queue) context referring to a queue (job) context that isn't stored yet.
func (repo *SchedulingContextRepository) AddSchedulingContext(sctx *schedulercontext.SchedulingContext) error {
	queueSchedulingContextByQueue, jobSchedulingContextByJobId := extractQueueAndJobContexts(sctx)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, jctx := range jobSchedulingContextByJobId {
		if err := repo.addJobSchedulingContext(jctx); err != nil {
			return err
		}
	}
	if err := repo.addQueueSchedulingContexts(maps.Values(queueSchedulingContextByQueue)); err != nil {
		return err
	}
	if err := repo.addSchedulingContext(sctx); err != nil {
		return err
	}
	if err := repo.addExecutorId(sctx.ExecutorId); err != nil {
		return err
	}
	return nil
}

// Should only be called from AddSchedulingContext to avoid concurrent and/or dirty writes.
func (repo *SchedulingContextRepository) addExecutorId(executorId string) error {
	n := len(repo.executorIds)
	repo.executorIds[executorId] = true
	if len(repo.executorIds) != n {
		sortedExecutorIds := maps.Keys(repo.executorIds)
		slices.Sort(sortedExecutorIds)
		repo.sortedExecutorIdsP.Store(&sortedExecutorIds)
	}
	return nil
}

// Should only be called from AddSchedulingContext to avoid dirty writes.
func (repo *SchedulingContextRepository) addSchedulingContext(sctx *schedulercontext.SchedulingContext) error {
	mostRecentSchedulingContextByExecutor := *repo.mostRecentSchedulingContextByExecutorP.Load()
	mostRecentSchedulingContextByExecutor = maps.Clone(mostRecentSchedulingContextByExecutor)
	mostRecentSchedulingContextByExecutor[sctx.ExecutorId] = sctx

	mostRecentSuccessfulSchedulingContextByExecutor := *repo.mostRecentSuccessfulSchedulingContextByExecutorP.Load()
	mostRecentSuccessfulSchedulingContextByExecutor = maps.Clone(mostRecentSuccessfulSchedulingContextByExecutor)
	if !sctx.ScheduledResourcesByPriority.IsZero() {
		mostRecentSuccessfulSchedulingContextByExecutor[sctx.ExecutorId] = sctx
	}

	mostRecentPreemptingContextByExecutor := *repo.mostRecentPreemptingSchedulingContextByExecutorP.Load()
	mostRecentPreemptingContextByExecutor = maps.Clone(mostRecentPreemptingContextByExecutor)
	if !sctx.EvictedResourcesByPriority.IsZero() {
		mostRecentPreemptingContextByExecutor[sctx.ExecutorId] = sctx
	}

	repo.mostRecentSchedulingContextByExecutorP.Store(&mostRecentSchedulingContextByExecutor)
	repo.mostRecentSuccessfulSchedulingContextByExecutorP.Store(&mostRecentSuccessfulSchedulingContextByExecutor)
	repo.mostRecentPreemptingSchedulingContextByExecutorP.Store(&mostRecentPreemptingContextByExecutor)

	return nil
}

// Should only be called from AddSchedulingContext to avoid dirty writes.
func (repo *SchedulingContextRepository) addQueueSchedulingContexts(qctxs []*schedulercontext.QueueSchedulingContext) error {
	mostRecentQueueSchedulingContextByExecutorByQueue := maps.Clone(*repo.mostRecentQueueSchedulingContextByExecutorByQueueP.Load())

	mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue := maps.Clone(*repo.mostRecentSuccessfulQueueSchedulingContextByExecutorByQueueP.Load())

	mostRecentPreemptingQueueSchedulingContextByExecutorByQueue := maps.Clone(*repo.mostRecentPreemptingQueueSchedulingContextByExecutorByQueueP.Load())

	for _, qctx := range qctxs {
		if qctx.ExecutorId == "" {
			return errors.WithStack(&armadaerrors.ErrInvalidArgument{
				Name:    "ExecutorId",
				Value:   "",
				Message: "received empty executorId",
			})
		}
		if qctx.Queue == "" {
			return errors.WithStack(&armadaerrors.ErrInvalidArgument{
				Name:    "Queue",
				Value:   "",
				Message: "received empty queue name",
			})
		}

		if previous := mostRecentQueueSchedulingContextByExecutorByQueue[qctx.Queue]; previous != nil {
			previous = maps.Clone(previous)
			previous[qctx.ExecutorId] = qctx
			mostRecentQueueSchedulingContextByExecutorByQueue[qctx.Queue] = previous
		} else {
			mostRecentQueueSchedulingContextByExecutorByQueue[qctx.Queue] = QueueSchedulingContextByExecutor{
				qctx.ExecutorId: qctx,
			}
		}

		if !qctx.EvictedResourcesByPriority.IsZero() {
			if previous := mostRecentPreemptingQueueSchedulingContextByExecutorByQueue[qctx.Queue]; previous != nil {
				previous = maps.Clone(previous)
				previous[qctx.ExecutorId] = qctx
				mostRecentPreemptingQueueSchedulingContextByExecutorByQueue[qctx.Queue] = previous
			} else {
				mostRecentPreemptingQueueSchedulingContextByExecutorByQueue[qctx.Queue] = QueueSchedulingContextByExecutor{
					qctx.ExecutorId: qctx,
				}
			}
		}

		if !qctx.ScheduledResourcesByPriority.IsZero() {
			if previous := mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue[qctx.Queue]; previous != nil {
				previous = maps.Clone(previous)
				previous[qctx.ExecutorId] = qctx
				mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue[qctx.Queue] = previous
			} else {
				mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue[qctx.Queue] = QueueSchedulingContextByExecutor{
					qctx.ExecutorId: qctx,
				}
			}
		}
	}

	repo.mostRecentQueueSchedulingContextByExecutorByQueueP.Store(&mostRecentQueueSchedulingContextByExecutorByQueue)
	repo.mostRecentSuccessfulQueueSchedulingContextByExecutorByQueueP.Store(&mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue)
	repo.mostRecentPreemptingQueueSchedulingContextByExecutorByQueueP.Store(&mostRecentPreemptingQueueSchedulingContextByExecutorByQueue)

	return nil
}

// Should only be called from AddSchedulingContext to avoid dirty writes.
func (repo *SchedulingContextRepository) addJobSchedulingContext(jctx *schedulercontext.JobSchedulingContext) error {
	if jctx.ExecutorId == "" {
		return errors.WithStack(&armadaerrors.ErrInvalidArgument{
			Name:    "ExecutorId",
			Value:   "",
			Message: "received empty executorId",
		})
	}
	if jctx.JobId == "" {
		return errors.WithStack(&armadaerrors.ErrInvalidArgument{
			Name:    "JobId",
			Value:   "",
			Message: "received empty jobId",
		})
	}
	previous, ok, _ := repo.mostRecentJobSchedulingContextByExecutorByJobId.PeekOrAdd(
		jctx.JobId,
		JobSchedulingContextByExecutor{jctx.ExecutorId: jctx},
	)
	if ok {
		jobSchedulingContextByExecutor := previous.(JobSchedulingContextByExecutor)
		jobSchedulingContextByExecutor[jctx.ExecutorId] = jctx
		repo.mostRecentJobSchedulingContextByExecutorByJobId.Add(jctx.JobId, jobSchedulingContextByExecutor)
	}
	return nil
}

// extractQueueAndJobContexts extracts the job and queue scheduling contexts from the scheduling context,
// and returns those separately.
func extractQueueAndJobContexts(sctx *schedulercontext.SchedulingContext) (map[string]*schedulercontext.QueueSchedulingContext, map[string]*schedulercontext.JobSchedulingContext) {
	queueSchedulingContextByQueue := make(map[string]*schedulercontext.QueueSchedulingContext)
	jobSchedulingContextByJobId := make(map[string]*schedulercontext.JobSchedulingContext)
	for queue, qctx := range sctx.QueueSchedulingContexts {
		for jobId, jctx := range qctx.SuccessfulJobSchedulingContexts {
			jobSchedulingContextByJobId[jobId] = jctx
		}
		for jobId, jctx := range qctx.UnsuccessfulJobSchedulingContexts {
			jobSchedulingContextByJobId[jobId] = jctx
		}
		queueSchedulingContextByQueue[queue] = qctx
	}
	return queueSchedulingContextByQueue, jobSchedulingContextByJobId
}

func (repo *SchedulingContextRepository) getSchedulingReportForQueue(queueName string) schedulingReport {
	mostRecent, _ := repo.GetMostRecentQueueSchedulingContextByExecutor(queueName)
	mostRecentSuccessful, _ := repo.GetMostRecentSuccessfulQueueSchedulingContextByExecutor(queueName)
	mostRecentPreempting, _ := repo.GetMostRecentPreemptingQueueSchedulingContextByExecutor(queueName)

	return schedulingReport{
		mostRecentSchedulingContextByExecutor:           armadamaps.MapValues(mostRecent, schedulercontext.GetSchedulingContextFromQueueSchedulingContext),
		mostRecentSuccessfulSchedulingContextByExecutor: armadamaps.MapValues(mostRecentSuccessful, schedulercontext.GetSchedulingContextFromQueueSchedulingContext),
		mostRecentPreemptingSchedulingContextByExecutor: armadamaps.MapValues(mostRecentPreempting, schedulercontext.GetSchedulingContextFromQueueSchedulingContext),

		sortedExecutorIds: repo.GetSortedExecutorIds(),
	}
}

func (repo *SchedulingContextRepository) getSchedulingReportForJob(jobId string) schedulingReport {
	mostRecent := make(map[string]*schedulercontext.QueueSchedulingContext)
	for _, byExecutor := range *repo.mostRecentQueueSchedulingContextByExecutorByQueueP.Load() {
		for executorId, qctx := range byExecutor {
			if existing, existed := mostRecent[executorId]; existed && qctx.Created.Before(existing.Created) {
				continue
			}
			_, successful := qctx.SuccessfulJobSchedulingContexts[jobId]
			_, unsuccessful := qctx.UnsuccessfulJobSchedulingContexts[jobId]
			_, preempted := qctx.EvictedJobsById[jobId]
			if successful || unsuccessful || preempted {
				mostRecent[executorId] = qctx
			}
		}
	}

	mostRecentSuccessful := make(map[string]*schedulercontext.QueueSchedulingContext)
	for _, byExecutor := range *repo.mostRecentSuccessfulQueueSchedulingContextByExecutorByQueueP.Load() {
		for executorId, qctx := range byExecutor {
			if existing, existed := mostRecentSuccessful[executorId]; existed && qctx.Created.Before(existing.Created) {
				continue
			}
			if _, successful := qctx.SuccessfulJobSchedulingContexts[jobId]; successful {
				mostRecentSuccessful[executorId] = qctx
			}
		}
	}

	mostRecentPreempting := make(map[string]*schedulercontext.QueueSchedulingContext)
	for _, byExecutor := range *repo.mostRecentPreemptingQueueSchedulingContextByExecutorByQueueP.Load() {
		for executorId, qctx := range byExecutor {
			if existing, existed := mostRecentPreempting[executorId]; existed && qctx.Created.Before(existing.Created) {
				continue
			}
			if _, preempted := qctx.EvictedJobsById[jobId]; preempted {
				mostRecentPreempting[executorId] = qctx
			}
		}
	}

	return schedulingReport{
		mostRecentSchedulingContextByExecutor:           armadamaps.MapValues(mostRecent, schedulercontext.GetSchedulingContextFromQueueSchedulingContext),
		mostRecentSuccessfulSchedulingContextByExecutor: armadamaps.MapValues(mostRecentSuccessful, schedulercontext.GetSchedulingContextFromQueueSchedulingContext),
		mostRecentPreemptingSchedulingContextByExecutor: armadamaps.MapValues(mostRecentPreempting, schedulercontext.GetSchedulingContextFromQueueSchedulingContext),

		sortedExecutorIds: repo.GetSortedExecutorIds(),
	}
}

func (repo *SchedulingContextRepository) getSchedulingReport() schedulingReport {
	return schedulingReport{
		mostRecentSchedulingContextByExecutor:           repo.GetMostRecentSchedulingContextByExecutor(),
		mostRecentSuccessfulSchedulingContextByExecutor: repo.GetMostRecentSuccessfulSchedulingContextByExecutor(),
		mostRecentPreemptingSchedulingContextByExecutor: repo.GetMostRecentPreemptingSchedulingContextByExecutor(),

		sortedExecutorIds: repo.GetSortedExecutorIds(),
	}
}

// GetSchedulingReport is a gRPC endpoint for querying scheduler reports.
// TODO: Further separate this from internal contexts.
func (repo *SchedulingContextRepository) GetSchedulingReport(_ context.Context, request *schedulerobjects.SchedulingReportRequest) (*schedulerobjects.SchedulingReport, error) {
	var sr schedulingReport

	switch filter := request.GetFilter().(type) {
	case *schedulerobjects.SchedulingReportRequest_MostRecentForQueue:
		queueName := strings.TrimSpace(filter.MostRecentForQueue.GetQueueName())
		sr = repo.getSchedulingReportForQueue(queueName)
	case *schedulerobjects.SchedulingReportRequest_MostRecentForJob:
		jobId := strings.TrimSpace(filter.MostRecentForJob.GetJobId())
		sr = repo.getSchedulingReportForJob(jobId)
	default:
		sr = repo.getSchedulingReport()
	}

	return &schedulerobjects.SchedulingReport{Report: sr.ReportString(request.GetVerbosity())}, nil
}

type schedulingReport struct {
	mostRecentSchedulingContextByExecutor           SchedulingContextByExecutor
	mostRecentSuccessfulSchedulingContextByExecutor SchedulingContextByExecutor
	mostRecentPreemptingSchedulingContextByExecutor SchedulingContextByExecutor

	sortedExecutorIds []string
}

func (sr schedulingReport) ReportString(verbosity int32) string {
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 1, 1, 1, ' ', 0)
	for _, executorId := range sr.sortedExecutorIds {
		fmt.Fprintf(w, "%s:\n", executorId)
		sctx := sr.mostRecentSchedulingContextByExecutor[executorId]
		if sctx != nil {
			fmt.Fprint(w, indent.String("\t", "Most recent attempt:\n"))
			fmt.Fprint(w, indent.String("\t\t", sctx.ReportString(verbosity)))
		} else {
			fmt.Fprint(w, indent.String("\t", "Most recent attempt: none\n"))
		}
		sctx = sr.mostRecentSuccessfulSchedulingContextByExecutor[executorId]
		if sctx != nil {
			fmt.Fprint(w, indent.String("\t", "Most recent successful attempt:\n"))
			fmt.Fprint(w, indent.String("\t\t", sctx.ReportString(verbosity)))
		} else {
			fmt.Fprint(w, indent.String("\t", "Most recent successful attempt: none\n"))
		}
		sctx = sr.mostRecentPreemptingSchedulingContextByExecutor[executorId]
		if sctx != nil {
			fmt.Fprint(w, indent.String("\t", "Most recent preempting attempt:\n"))
			fmt.Fprint(w, indent.String("\t\t", sctx.ReportString(verbosity)))
		} else {
			fmt.Fprint(w, indent.String("\t", "Most recent preempting attempt: none\n"))
		}
	}
	w.Flush()
	return sb.String()
}

// GetQueueReport is a gRPC endpoint for querying queue reports.
// TODO: Further separate this from internal contexts.
func (repo *SchedulingContextRepository) GetQueueReport(_ context.Context, request *schedulerobjects.QueueReportRequest) (*schedulerobjects.QueueReport, error) {
	queueName := strings.TrimSpace(request.GetQueueName())
	verbosity := request.GetVerbosity()
	return &schedulerobjects.QueueReport{
		Report: repo.getQueueReportString(queueName, verbosity),
	}, nil
}

func (repo *SchedulingContextRepository) getQueueReportString(queue string, verbosity int32) string {
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 1, 1, 1, ' ', 0)
	sortedExecutorIds := repo.GetSortedExecutorIds()
	mostRecentQueueSchedulingContextByExecutor, _ := repo.GetMostRecentQueueSchedulingContextByExecutor(queue)
	mostRecentSuccessfulQueueSchedulingContextByExecutor, _ := repo.GetMostRecentSuccessfulQueueSchedulingContextByExecutor(queue)
	mostRecentPreemptingQueueSchedulingContextByExecutor, _ := repo.GetMostRecentPreemptingQueueSchedulingContextByExecutor(queue)
	for _, executorId := range sortedExecutorIds {
		fmt.Fprintf(w, "%s:\n", executorId)
		qctx := mostRecentQueueSchedulingContextByExecutor[executorId]
		if qctx != nil {
			fmt.Fprint(w, indent.String("\t", "Most recent attempt:\n"))
			fmt.Fprint(w, indent.String("\t\t", qctx.ReportString(verbosity)))
		} else {
			fmt.Fprint(w, indent.String("\t", "Most recent attempt: none\n"))
		}
		qctx = mostRecentSuccessfulQueueSchedulingContextByExecutor[executorId]
		if qctx != nil {
			fmt.Fprint(w, indent.String("\t", "Most recent successful attempt:\n"))
			fmt.Fprint(w, indent.String("\t\t", qctx.ReportString(verbosity)))
		} else {
			fmt.Fprint(w, indent.String("\t", "Most recent successful attempt: none\n"))
		}
		qctx = mostRecentPreemptingQueueSchedulingContextByExecutor[executorId]
		if qctx != nil {
			fmt.Fprint(w, indent.String("\t", "Most recent preempting attempt:\n"))
			fmt.Fprint(w, indent.String("\t\t", qctx.ReportString(verbosity)))
		} else {
			fmt.Fprint(w, indent.String("\t", "Most recent preempting attempt: none\n"))
		}
	}
	w.Flush()
	return sb.String()
}

// GetJobReport is a gRPC endpoint for querying job reports.
// TODO: Further separate this from internal contexts.
func (repo *SchedulingContextRepository) GetJobReport(_ context.Context, request *schedulerobjects.JobReportRequest) (*schedulerobjects.JobReport, error) {
	jobId := strings.TrimSpace(request.GetJobId())
	if _, err := ulid.Parse(jobId); err != nil {
		return nil, &armadaerrors.ErrInvalidArgument{
			Name:    "jobId",
			Value:   request.GetJobId(),
			Message: fmt.Sprintf("%s is not a valid jobId", request.GetJobId()),
		}
	}
	return &schedulerobjects.JobReport{
		Report: repo.getJobReportString(jobId),
	}, nil
}

func (repo *SchedulingContextRepository) getJobReportString(jobId string) string {
	sortedExecutorIds := repo.GetSortedExecutorIds()
	jobSchedulingContextByExecutor, _ := repo.GetMostRecentJobSchedulingContextByExecutor(jobId)
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 1, 1, 1, ' ', 0)
	for _, executorId := range sortedExecutorIds {
		jctx := jobSchedulingContextByExecutor[executorId]
		if jctx != nil {
			fmt.Fprintf(w, "%s:\n", executorId)
			fmt.Fprint(w, indent.String("\t", jctx.String()))
		} else {
			fmt.Fprintf(w, "%s: no recent attempt\n", executorId)
		}
	}
	w.Flush()
	return sb.String()
}

func (repo *SchedulingContextRepository) GetMostRecentSchedulingContextByExecutor() SchedulingContextByExecutor {
	return *repo.mostRecentSchedulingContextByExecutorP.Load()
}

func (repo *SchedulingContextRepository) GetMostRecentSuccessfulSchedulingContextByExecutor() SchedulingContextByExecutor {
	return *repo.mostRecentSuccessfulSchedulingContextByExecutorP.Load()
}

func (repo *SchedulingContextRepository) GetMostRecentPreemptingSchedulingContextByExecutor() SchedulingContextByExecutor {
	return *repo.mostRecentPreemptingSchedulingContextByExecutorP.Load()
}

func (repo *SchedulingContextRepository) GetMostRecentQueueSchedulingContextByExecutor(queue string) (QueueSchedulingContextByExecutor, bool) {
	mostRecentQueueSchedulingContextByExecutorByQueue := *repo.mostRecentQueueSchedulingContextByExecutorByQueueP.Load()
	mostRecentQueueSchedulingContextByExecutor, ok := mostRecentQueueSchedulingContextByExecutorByQueue[queue]
	return mostRecentQueueSchedulingContextByExecutor, ok
}

func (repo *SchedulingContextRepository) GetMostRecentSuccessfulQueueSchedulingContextByExecutor(queue string) (QueueSchedulingContextByExecutor, bool) {
	mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue := *repo.mostRecentSuccessfulQueueSchedulingContextByExecutorByQueueP.Load()
	mostRecentSuccessfulQueueSchedulingContextByExecutor, ok := mostRecentSuccessfulQueueSchedulingContextByExecutorByQueue[queue]
	return mostRecentSuccessfulQueueSchedulingContextByExecutor, ok
}

func (repo *SchedulingContextRepository) GetMostRecentPreemptingQueueSchedulingContextByExecutor(queue string) (QueueSchedulingContextByExecutor, bool) {
	mostRecentPreemptingQueueSchedulingContextByExecutorByQueue := *repo.mostRecentPreemptingQueueSchedulingContextByExecutorByQueueP.Load()
	mostRecentPreemptingQueueSchedulingContextByExecutor, ok := mostRecentPreemptingQueueSchedulingContextByExecutorByQueue[queue]
	return mostRecentPreemptingQueueSchedulingContextByExecutor, ok
}

func (repo *SchedulingContextRepository) GetMostRecentJobSchedulingContextByExecutor(jobId string) (JobSchedulingContextByExecutor, bool) {
	if v, ok := repo.mostRecentJobSchedulingContextByExecutorByJobId.Get(jobId); ok {
		jobSchedulingContextByExecutor := v.(JobSchedulingContextByExecutor)
		return jobSchedulingContextByExecutor, true
	} else {
		return nil, false
	}
}

func (repo *SchedulingContextRepository) GetSortedExecutorIds() []string {
	return *repo.sortedExecutorIdsP.Load()
}

func (m SchedulingContextByExecutor) String() string {
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 1, 1, 1, ' ', 0)
	executorIds := maps.Keys(m)
	slices.Sort(executorIds)
	for _, executorId := range executorIds {
		sctx := m[executorId]
		fmt.Fprintf(w, "%s:\n", executorId)
		fmt.Fprint(w, indent.String("\t", sctx.String()))
	}
	w.Flush()
	return sb.String()
}

func (m QueueSchedulingContextByExecutor) String() string {
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 1, 1, 1, ' ', 0)
	executorIds := maps.Keys(m)
	slices.Sort(executorIds)
	for _, executorId := range executorIds {
		qctx := m[executorId]
		fmt.Fprintf(w, "%s:\n", executorId)
		fmt.Fprint(w, indent.String("\t", qctx.String()))
	}
	w.Flush()
	return sb.String()
}

func (m JobSchedulingContextByExecutor) String() string {
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 1, 1, 1, ' ', 0)
	executorIds := maps.Keys(m)
	slices.Sort(executorIds)
	for _, executorId := range executorIds {
		jctx := m[executorId]
		fmt.Fprintf(w, "%s:\n", executorId)
		fmt.Fprint(w, indent.String("\t", jctx.String()))
	}
	w.Flush()
	return sb.String()
}
