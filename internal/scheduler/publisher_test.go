//go:generate mockgen -destination=./mock_pulsar.go -package=scheduler "github.com/apache/pulsar-client-go/pulsar" Client,Producer
package scheduler

import (
	"context"
	"fmt"
	"github.com/G-Research/armada/internal/common/pulsarutils"
	"github.com/G-Research/armada/pkg/armadaevents"
	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"math"
	"testing"
	"time"
)

const (
	topic          = "testTopic"
	numPartititons = 100
	messageSize    = 1024 // 1kb
)

func TestPulsarPublisher_TestPublish(t *testing.T) {

	tests := map[string]struct {
		eventSequences        []*armadaevents.EventSequence
		numSucessfulPublishes int
		amLeader              bool
		expectedError         bool
	}{
		"Publish if leader": {
			amLeader:              true,
			numSucessfulPublishes: math.MaxInt,
			eventSequences: []*armadaevents.EventSequence{
				{
					JobSetName: "jobset1",
					Events:     []*armadaevents.EventSequence_Event{{}, {}},
				},
				{
					JobSetName: "jobset1",
					Events:     []*armadaevents.EventSequence_Event{{}},
				},
				{
					JobSetName: "jobset2",
					Events:     []*armadaevents.EventSequence_Event{{}},
				},
			},
		},
		"Don't publish if not leader": {
			amLeader:              false,
			numSucessfulPublishes: math.MaxInt,
			eventSequences: []*armadaevents.EventSequence{
				{
					JobSetName: "jobset1",
					Events:     []*armadaevents.EventSequence_Event{{}, {}},
				},
			},
		},
		"Return error if all events fail to publish": {
			amLeader:              true,
			numSucessfulPublishes: 0,
			eventSequences: []*armadaevents.EventSequence{
				{
					JobSetName: "jobset1",
					Events:     []*armadaevents.EventSequence_Event{{}},
				},
			},
			expectedError: true,
		},
		"Return error if some events fail to publish": {
			amLeader:              true,
			numSucessfulPublishes: 1,
			eventSequences: []*armadaevents.EventSequence{
				{
					JobSetName: "jobset1",
					Events:     []*armadaevents.EventSequence_Event{{}},
				},
				{
					JobSetName: "jobset2",
					Events:     []*armadaevents.EventSequence_Event{{}},
				},
			},
			expectedError: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockPulsarClient := NewMockClient(ctrl)
			mockPulsarProducer := NewMockProducer(ctrl)
			mockPulsarClient.EXPECT().CreateProducer(gomock.Any()).Return(mockPulsarProducer, nil).Times(1)
			mockPulsarClient.EXPECT().TopicPartitions(topic).Return(make([]string, numPartititons), nil)
			leaderController := NewStandaloneLeaderController()
			var numPublished = 0
			var capturedEvents []*armadaevents.EventSequence
			expectedCounts := make(map[string]int)
			if tc.amLeader {
				expectedCounts = countEvents(tc.eventSequences)
			}

			mockPulsarProducer.
				EXPECT().
				SendAsync(gomock.Any(), gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, msg *pulsar.ProducerMessage, callback func(pulsar.MessageID, *pulsar.ProducerMessage, error)) {
					es := &armadaevents.EventSequence{}
					proto.Unmarshal(msg.Payload, es)
					capturedEvents = append(capturedEvents, es)
					numPublished++
					if numPublished > tc.numSucessfulPublishes {
						callback(pulsarutils.NewMessageId(numPublished), msg, errors.New("error from mock pulsar producer"))
					} else {
						callback(pulsarutils.NewMessageId(numPublished), msg, nil)
					}
				}).AnyTimes()

			options := pulsar.ProducerOptions{Topic: topic}
			ctx := context.TODO()
			publisher, err := NewPulsarPublisher(mockPulsarClient, options, leaderController, 5*time.Second, messageSize)
			require.NoError(t, err)
			token := leaderController.GetToken()
			if !tc.amLeader {
				token = InvalidLeaderToken()
			}

			err = publisher.PublishMessages(ctx, tc.eventSequences, token)

			// Check that we get an error if one is expected
			if tc.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			// Check that we got the messages that we expect
			if tc.amLeader {
				capturedCounts := countEvents(capturedEvents)
				assert.Equal(t, expectedCounts, capturedCounts)
			}
		})
	}
}

func TestPulsarPublisher_TestPublishMarkers(t *testing.T) {
	allPartitions := make(map[string]bool, 0)
	for i := 0; i < numPartititons; i++ {
		allPartitions[fmt.Sprintf("%d", i)] = true
	}
	tests := map[string]struct {
		numSucessfulPublishes int
		expectedError         bool
		expectedPartitons     map[string]bool
	}{
		"Publish successful": {
			numSucessfulPublishes: math.MaxInt,
			expectedError:         false,
			expectedPartitons:     allPartitions,
		},
		"All Publishes fail": {
			numSucessfulPublishes: 0,
			expectedError:         true,
		},
		"Some Publishes fail": {
			numSucessfulPublishes: 10,
			expectedError:         true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockPulsarClient := NewMockClient(ctrl)
			mockPulsarProducer := NewMockProducer(ctrl)
			mockPulsarClient.EXPECT().CreateProducer(gomock.Any()).Return(mockPulsarProducer, nil).Times(1)
			mockPulsarClient.EXPECT().TopicPartitions(topic).Return(make([]string, numPartititons), nil)
			leaderController := NewStandaloneLeaderController()
			var numPublished = 0
			var capturedPartitons = make(map[string]bool)

			mockPulsarProducer.
				EXPECT().
				Send(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, msg *pulsar.ProducerMessage) (pulsar.MessageID, error) {
					numPublished++
					key, ok := msg.Properties[explicitPartitionKey]
					if ok {
						capturedPartitons[key] = true
					}
					if numPublished > tc.numSucessfulPublishes {
						log.Info("returning error")
						return pulsarutils.NewMessageId(numPublished), errors.New("error from mock pulsar producer")
					}
					return pulsarutils.NewMessageId(numPublished), nil
				}).AnyTimes()

			options := pulsar.ProducerOptions{Topic: topic}
			ctx := context.TODO()
			publisher, err := NewPulsarPublisher(mockPulsarClient, options, leaderController, 5*time.Second, messageSize)
			require.NoError(t, err)

			published, err := publisher.PublishMarkers(ctx, uuid.New())

			// Check that we get an error if one is expected
			if tc.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if !tc.expectedError {
				assert.Equal(t, uint32(numPartititons), published)
				assert.Equal(t, tc.expectedPartitons, capturedPartitons)
			}
		})
	}
}

func countEvents(es []*armadaevents.EventSequence) map[string]int {
	countsById := make(map[string]int)
	for _, sequence := range es {
		jobset := sequence.JobSetName
		count := countsById[jobset]
		count += len(sequence.Events)
		countsById[jobset] = count
	}
	return countsById
}
