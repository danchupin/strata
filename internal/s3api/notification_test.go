package s3api_test

import (
	"strings"
	"testing"
)

const notificationTopicXML = `<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<TopicConfiguration>
		<Id>TopicOne</Id>
		<Topic>arn:aws:sns:us-east-1:123456789012:test</Topic>
		<Event>s3:ObjectCreated:*</Event>
	</TopicConfiguration>
</NotificationConfiguration>`

const notificationQueueXML = `<NotificationConfiguration>
	<QueueConfiguration>
		<Id>QueueOne</Id>
		<Queue>arn:aws:sqs:us-east-1:123456789012:test</Queue>
		<Event>s3:ObjectRemoved:*</Event>
	</QueueConfiguration>
</NotificationConfiguration>`

const notificationLambdaXML = `<NotificationConfiguration>
	<CloudFunctionConfiguration>
		<Id>FuncOne</Id>
		<CloudFunction>arn:aws:lambda:us-east-1:123456789012:function:test</CloudFunction>
		<Event>s3:ObjectCreated:Put</Event>
	</CloudFunctionConfiguration>
</NotificationConfiguration>`

func TestBucketNotificationCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// GET on fresh bucket → 404 NoSuchConfiguration.
	resp := h.doString("GET", "/bkt?notification=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchConfiguration") {
		t.Fatalf("expected NoSuchConfiguration, got: %s", body)
	}

	// PUT a TopicConfiguration round-trips.
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notificationTopicXML), 200)

	resp = h.doString("GET", "/bkt?notification=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "TopicConfiguration") || !strings.Contains(body, "TopicOne") {
		t.Fatalf("GET notification body missing topic: %s", body)
	}
}

func TestBucketNotificationQueueRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notificationQueueXML), 200)
	resp := h.doString("GET", "/bkt?notification=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "QueueConfiguration") || !strings.Contains(body, "QueueOne") {
		t.Fatalf("GET notification body missing queue: %s", body)
	}
}

func TestBucketNotificationLambdaRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notificationLambdaXML), 200)
	resp := h.doString("GET", "/bkt?notification=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "CloudFunctionConfiguration") {
		t.Fatalf("GET notification body missing lambda: %s", body)
	}
}

func TestBucketNotificationMalformedBody(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?notification=", "<NotificationConfiguration><nope")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedXML") {
		t.Fatalf("expected MalformedXML, got: %s", body)
	}
}

func TestBucketNotificationEmptyConfigRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// Well-formed XML but no Topic/Queue/Lambda/EventBridge → 400.
	resp := h.doString("PUT", "/bkt?notification=",
		"<NotificationConfiguration></NotificationConfiguration>")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedXML") {
		t.Fatalf("expected MalformedXML, got: %s", body)
	}
}

func TestBucketNotificationEmptyBodyClears(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notificationTopicXML), 200)

	// Empty body PUT clears the configuration.
	h.mustStatus(h.doString("PUT", "/bkt?notification=", ""), 200)

	resp := h.doString("GET", "/bkt?notification=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchConfiguration") {
		t.Fatalf("expected NoSuchConfiguration after clear, got: %s", body)
	}
}

func TestBucketNotificationOnMissingBucket(t *testing.T) {
	h := newHarness(t)
	resp := h.doString("GET", "/missing?notification=", "")
	h.mustStatus(resp, 404)
}
