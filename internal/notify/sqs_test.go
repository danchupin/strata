package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/danchupin/strata/internal/meta"
)

type fakeSQSClient struct {
	inputs []*sqs.SendMessageInput
	err    error
}

func (f *fakeSQSClient) SendMessage(ctx context.Context, in *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sqs.SendMessageOutput{MessageId: aws.String("msg-1")}, nil
}

func TestSQSSinkSendsPayloadAsMessageBody(t *testing.T) {
	body := []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put"}]}`)
	client := &fakeSQSClient{}
	sink := &SQSSink{
		SinkName: "s",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789012/q",
		Region:   "us-east-1",
		Client:   client,
	}
	evt := meta.NotificationEvent{EventID: "e1", EventName: "s3:ObjectCreated:Put", Payload: body}
	if err := sink.Send(context.Background(), evt); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(client.inputs) != 1 {
		t.Fatalf("inputs=%d want 1", len(client.inputs))
	}
	in := client.inputs[0]
	if aws.ToString(in.QueueUrl) != sink.QueueURL {
		t.Fatalf("QueueUrl: %q", aws.ToString(in.QueueUrl))
	}
	if aws.ToString(in.MessageBody) != string(body) {
		t.Fatalf("MessageBody: %q", aws.ToString(in.MessageBody))
	}
}

func TestSQSSinkPropagatesError(t *testing.T) {
	client := &fakeSQSClient{err: errors.New("AccessDeniedException")}
	sink := &SQSSink{SinkName: "s", QueueURL: "https://q", Client: client}
	err := sink.Send(context.Background(), meta.NotificationEvent{Payload: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, client.err) {
		t.Fatalf("error %v should wrap %v", err, client.err)
	}
}

func TestSQSSinkRejectsMissingQueueURL(t *testing.T) {
	sink := &SQSSink{SinkName: "s", Client: &fakeSQSClient{}}
	if err := sink.Send(context.Background(), meta.NotificationEvent{Payload: []byte(`{}`)}); err == nil {
		t.Fatal("missing queue URL should error")
	}
}

func TestSQSSinkRejectsMissingClient(t *testing.T) {
	sink := &SQSSink{SinkName: "s", QueueURL: "https://q"}
	if err := sink.Send(context.Background(), meta.NotificationEvent{Payload: []byte(`{}`)}); err == nil {
		t.Fatal("missing client should error")
	}
}

func TestSQSSinkTypeName(t *testing.T) {
	sink := &SQSSink{SinkName: "alpha"}
	if sink.Type() != "sqs" {
		t.Fatalf("type: %q", sink.Type())
	}
	if sink.Name() != "alpha" {
		t.Fatalf("name: %q", sink.Name())
	}
}
