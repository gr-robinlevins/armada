package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/armadaproject/armada/internal/armada/configuration"
	schedulerconstraints "github.com/armadaproject/armada/internal/scheduler/constraints"
	schedulercontext "github.com/armadaproject/armada/internal/scheduler/context"
	"github.com/armadaproject/armada/internal/scheduler/jobdb"
	"github.com/armadaproject/armada/internal/scheduler/nodedb"
	"github.com/armadaproject/armada/internal/scheduler/schedulerobjects"
	"github.com/armadaproject/armada/internal/scheduler/testfixtures"
)

func TestGangScheduler(t *testing.T) {
	tests := map[string]struct {
		SchedulingConfig configuration.SchedulingConfig
		// Minimum job size.
		MinimumJobSize map[string]resource.Quantity
		// Nodes to be considered by the scheduler.
		Nodes []*schedulerobjects.Node
		// Total resources across all clusters.
		// Set to the total resources across all nodes if not provided.
		TotalResources schedulerobjects.ResourceList
		// Gangs to try scheduling.
		Gangs [][]*jobdb.Job
		// Indices of gangs expected to be scheduled.
		ExpectedScheduledIndices []int
	}{
		"simple success": {
			SchedulingConfig: testfixtures.TestSchedulingConfig(),
			Nodes:            testfixtures.N32CpuNodes(1, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 32),
			},
			ExpectedScheduledIndices: testfixtures.IntRange(0, 0),
		},
		"simple failure": {
			SchedulingConfig: testfixtures.TestSchedulingConfig(),
			Nodes:            testfixtures.N32CpuNodes(1, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 33),
			},
			ExpectedScheduledIndices: nil,
		},
		"one success and one failure": {
			SchedulingConfig: testfixtures.TestSchedulingConfig(),
			Nodes:            testfixtures.N32CpuNodes(1, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 32),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
			},
			ExpectedScheduledIndices: testfixtures.IntRange(0, 0),
		},
		"multiple nodes": {
			SchedulingConfig: testfixtures.TestSchedulingConfig(),
			Nodes:            testfixtures.N32CpuNodes(2, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 64),
			},
			ExpectedScheduledIndices: testfixtures.IntRange(0, 0),
		},
		"MaximumResourceFractionToSchedule": {
			SchedulingConfig: testfixtures.WithRoundLimitsConfig(
				map[string]float64{"cpu": 0.5},
				testfixtures.TestSchedulingConfig(),
			),
			Nodes: testfixtures.N32CpuNodes(1, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 8),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 16),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 8),
			},
			ExpectedScheduledIndices: []int{0, 1},
		},
		"MaximumResourceFractionToScheduleByPool": {
			SchedulingConfig: testfixtures.WithRoundLimitsConfig(
				map[string]float64{"cpu": 0.5},
				testfixtures.WithRoundLimitsPoolConfig(
					map[string]map[string]float64{"pool": {"cpu": 2.0 / 32.0}},
					testfixtures.TestSchedulingConfig(),
				),
			),
			Nodes: testfixtures.N32CpuNodes(1, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
			},
			ExpectedScheduledIndices: []int{0, 1, 2},
		},
		"MaximumResourceFractionToScheduleByPool non-existing pool": {
			SchedulingConfig: testfixtures.WithRoundLimitsConfig(
				map[string]float64{"cpu": 3.0 / 32.0},
				testfixtures.WithRoundLimitsPoolConfig(
					map[string]map[string]float64{"this does not exist": {"cpu": 2.0 / 32.0}},
					testfixtures.TestSchedulingConfig(),
				),
			),
			Nodes: testfixtures.N32CpuNodes(1, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 1),
			},
			ExpectedScheduledIndices: []int{0, 1, 2, 3},
		},
		"MaximumResourceFractionPerQueue": {
			SchedulingConfig: testfixtures.WithPerPriorityLimitsConfig(
				map[int32]map[string]float64{
					0: {"cpu": 1.0},
					1: {"cpu": 15.0 / 32.0},
					2: {"cpu": 10.0 / 32.0},
					3: {"cpu": 3.0 / 32.0},
				},
				testfixtures.TestSchedulingConfig(),
			),
			Nodes: testfixtures.N32CpuNodes(1, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass3, 4),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass3, 3),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass2, 8),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass2, 7),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass1, 6),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass1, 5),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 18),
				testfixtures.N1CpuJobs("A", testfixtures.PriorityClass0, 17),
			},
			ExpectedScheduledIndices: []int{1, 3, 5, 7},
		},
		"resolution has no impact on jobs of size a multiple of the resolution": {
			SchedulingConfig: testfixtures.WithIndexedResourcesConfig(
				[]configuration.IndexedResource{
					{Name: "cpu", Resolution: resource.MustParse("16")},
					{Name: "memory", Resolution: resource.MustParse("128Mi")},
				},
				testfixtures.TestSchedulingConfig(),
			),
			Nodes: testfixtures.N32CpuNodes(3, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
			},
			ExpectedScheduledIndices: testfixtures.IntRange(0, 5),
		},
		"jobs of size not a multiple of the resolution blocks scheduling new jobs": {
			SchedulingConfig: testfixtures.WithIndexedResourcesConfig(
				[]configuration.IndexedResource{
					{Name: "cpu", Resolution: resource.MustParse("17")},
					{Name: "memory", Resolution: resource.MustParse("128Mi")},
				},
				testfixtures.TestSchedulingConfig(),
			),
			Nodes: testfixtures.N32CpuNodes(3, testfixtures.TestPriorities),
			Gangs: [][]*jobdb.Job{
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
				testfixtures.N16CpuJobs("A", testfixtures.PriorityClass0, 1),
			},
			ExpectedScheduledIndices: testfixtures.IntRange(0, 2),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			nodeDb, err := nodedb.NewNodeDb(
				testfixtures.TestPriorityClasses,
				testfixtures.TestMaxExtraNodesToConsider,
				tc.SchedulingConfig.IndexedResources,
				testfixtures.TestIndexedTaints,
				testfixtures.TestIndexedNodeLabels,
			)
			require.NoError(t, err)
			err = nodeDb.UpsertMany(tc.Nodes)
			require.NoError(t, err)
			if tc.TotalResources.Resources == nil {
				// Default to NodeDb total.
				tc.TotalResources = nodeDb.TotalResources()
			}
			priorityFactorByQueue := make(map[string]float64)
			for _, jobs := range tc.Gangs {
				for _, job := range jobs {
					priorityFactorByQueue[job.GetQueue()] = 1
				}
			}
			sctx := schedulercontext.NewSchedulingContext(
				"executor",
				"pool",
				tc.SchedulingConfig.Preemption.PriorityClasses,
				tc.SchedulingConfig.Preemption.DefaultPriorityClass,
				tc.SchedulingConfig.ResourceScarcity,
				tc.TotalResources,
			)
			for queue, priorityFactor := range priorityFactorByQueue {
				err := sctx.AddQueueSchedulingContext(queue, priorityFactor, nil)
				require.NoError(t, err)
			}
			constraints := schedulerconstraints.SchedulingConstraintsFromSchedulingConfig(
				"pool",
				tc.TotalResources,
				schedulerobjects.ResourceList{Resources: tc.MinimumJobSize},
				tc.SchedulingConfig,
			)
			sch, err := NewGangScheduler(sctx, constraints, nodeDb)
			require.NoError(t, err)

			var actualScheduledIndices []int
			for i, gang := range tc.Gangs {
				jctxs := jobSchedulingContextsFromJobs(gang, "", testfixtures.TestPriorityClasses)
				gctx := schedulercontext.NewGangSchedulingContext(jctxs)
				ok, reason, err := sch.Schedule(context.Background(), gctx)
				require.NoError(t, err)
				if ok {
					require.Empty(t, reason)
					actualScheduledIndices = append(actualScheduledIndices, i)
				} else {
					require.NotEmpty(t, reason)
				}
			}
			assert.Equal(t, tc.ExpectedScheduledIndices, actualScheduledIndices)
		})
	}
}
