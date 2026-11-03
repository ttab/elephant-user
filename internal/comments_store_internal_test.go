package internal

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ttab/elephantine/test"
)

func TestMentionedSubjects(t *testing.T) {
	payload := json.RawMessage(`{
	  "uuid": "6f1d1d8e-2b1a-4c55-9d7e-3a2f4b5c6d7e",
	  "type": "core/comment",
	  "content": [
	    {"type": "core/text", "links": [
	      {"id": "m1", "rel": "mention", "uri": "core://user/b"},
	      {"id": "l1", "rel": "see-also", "uri": "core://user/x",
	       "uuid": "9c1b8d0e-6f4a-4b8e-8a8f-0d2c3b4a5e6f"},
	      {"id": "m2", "rel": "mention", "uri": "core://application/a"}
	    ]},
	    {"type": "core/text", "links": [
	      {"id": "m3", "rel": "mention", "uri": "core://user/b"}
	    ]},
	    {"type": "core/text"}
	  ],
	  "links": [
	    {"rel": "mention", "uri": "core://user/top-level-is-not-content"}
	  ]
	}`)

	got, err := mentionedSubjects(payload)
	test.Mustf(t, err, "derive mentions")

	want := []string{"core://application/a", "core://user/b"}

	if !slices.Equal(want, got) {
		t.Fatalf("got mentions %v, want %v", got, want)
	}

	none, err := mentionedSubjects(json.RawMessage(`{"type":"core/comment"}`))
	test.Mustf(t, err, "derive mentions from a payload without links")

	if none == nil || len(none) != 0 {
		t.Fatalf("got %#v for a payload without mentions, want an empty non-nil slice", none)
	}

	_, err = mentionedSubjects(json.RawMessage(`[`))
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestFitNotifyPayload(t *testing.T) {
	threadUUID := uuid.New()

	event := CommentEvent{
		Type:         CommentEventCommentCreated,
		DocumentUUID: uuid.New(),
		ThreadUUID:   threadUUID,
		CommentUUID:  &threadUUID,
		Actor:        "core://user/actor",
		Kind:         "factcheck",
	}

	for i := range 500 {
		event.Mentioned = append(event.Mentioned,
			fmt.Sprintf("core://user/%s-%04d", strings.Repeat("x", 20), i))
	}

	fitted, err := fitNotifyPayload(event)
	test.Mustf(t, err, "fit the payload")

	data, err := json.Marshal(fitted)
	test.Mustf(t, err, "marshal the fitted event")

	if len(data) > maxNotifyPayload {
		t.Fatalf("fitted payload is %d bytes, over the %d limit",
			len(data), maxNotifyPayload)
	}

	n := len(fitted.Mentioned)
	if n == 0 || n == len(event.Mentioned) {
		t.Fatalf("kept %d of %d mentions, want a truncated prefix",
			n, len(event.Mentioned))
	}

	if !slices.Equal(event.Mentioned[:n], fitted.Mentioned) {
		t.Fatal("the kept mentions are not a prefix of the sorted list")
	}

	// One more mention would not have fit.
	fitted.Mentioned = event.Mentioned[:n+1]

	data, err = json.Marshal(fitted)
	test.Mustf(t, err, "marshal the event with one more mention")

	if len(data) <= maxNotifyPayload {
		t.Fatalf("truncated more than needed: %d bytes with one more mention",
			len(data))
	}

	small := event
	small.Mentioned = event.Mentioned[:3]

	same, err := fitNotifyPayload(small)
	test.Mustf(t, err, "fit a small payload")

	test.Equalf(t, 3, len(same.Mentioned), "a payload that fits is left alone")
}

func TestSameJSON(t *testing.T) {
	same, err := sameJSON(
		[]byte(`{"a": 1, "b": {"c": "x", "d": [1, 2]}}`),
		[]byte(`{"b":{"d":[1,2],"c":"x"},"a":1}`))
	test.Mustf(t, err, "compare")
	test.Equalf(t, true, same, "key order and whitespace do not matter")

	same, err = sameJSON([]byte(`{"a": [1, 2]}`), []byte(`{"a": [2, 1]}`))
	test.Mustf(t, err, "compare")
	test.Equalf(t, false, same, "array order matters")
}
