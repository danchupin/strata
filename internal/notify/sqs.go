package notify

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/danchupin/strata/internal/meta"
)

// SQSAPI is the subset of *sqs.Client used by SQSSink. Defining it as an
// interface lets unit tests swap in a mock without spinning up a real SDK
// client. The cmd/strata/workers/notify.go wiring passes a real *sqs.Client
// built from config.LoadDefaultConfig (which honours the standard AWS
// credential chain: env vars, shared config, IRSA, and EC2/ECS instance roles).
type SQSAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// SQSSink delivers events to an SQS queue via the AWS SDK's SendMessage. The
// event Payload (a meta.NotificationEvent.Payload, AWS S3-event-message JSON
// built in s3api.buildNotificationPayload) is the literal MessageBody — no
// re-marshal — so consumers see the same bytes that a webhook subscriber
// would.
type SQSSink struct {
	SinkName string
	QueueURL string
	Region   string
	Client   SQSAPI
}

func (s *SQSSink) Type() string { return "sqs" }
func (s *SQSSink) Name() string { return s.SinkName }

func (s *SQSSink) Send(ctx context.Context, evt meta.NotificationEvent) error {
	if s.QueueURL == "" {
		return errors.New("sqs: QueueURL not configured")
	}
	if s.Client == nil {
		return errors.New("sqs: client not configured")
	}
	body := string(evt.Payload)
	_, err := s.Client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(s.QueueURL),
		MessageBody: aws.String(body),
	})
	if err != nil {
		return fmt.Errorf("sqs: SendMessage to %s: %w", s.QueueURL, err)
	}
	return nil
}
