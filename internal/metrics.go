package internal

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
)

// Metric label names.
const (
	LabelDeprecation = "label"
	LabelDocType     = "doc_type"
	LabelOutcome     = "outcome"
	LabelFamily      = "family"
	LabelItem        = "item"
	LabelReason      = "reason"
)

// Why a stream sent a notification.
const (
	// StreamPushReplay is a row sent from the cursor when the stream
	// opened.
	StreamPushReplay = "replay"
	// StreamPushLive is a row sent as it was written, or re-read after a
	// dropped wakeup.
	StreamPushLive = "live"
)

// NotificationFamilyOther labels feed rows from a family the metrics do not
// know, so that a caller-supplied value never becomes a label.
const NotificationFamilyOther = "other"

// What the cleaner deleted.
const (
	// CleanerDeletedSubscription is an expired notification subscription.
	CleanerDeletedSubscription = "subscription"
	// CleanerDeletedNotification is a feed row past the retention.
	CleanerDeletedNotification = "notification"
)

// Outcomes of a document permission lookup.
const (
	// PermissionLookupHit is an answer served from the cache.
	PermissionLookupHit = "hit"
	// PermissionLookupMiss is an answer the repository gave.
	PermissionLookupMiss = "miss"
	// PermissionLookupDenied is a token the repository refused outright.
	PermissionLookupDenied = "denied"
	// PermissionLookupNotFound is a document the repository does not know.
	PermissionLookupNotFound = "not_found"
	// PermissionLookupError is a lookup that failed.
	PermissionLookupError = "error"
)

// Metrics holds every Prometheus collector the service owns. It is
// constructed once with NewMetrics and handed to the subsystems that
// report through it.
type Metrics struct {
	// Deprecations counts uses of deprecated schema constructs by
	// deprecation label.
	Deprecations *prometheus.CounterVec
	// DocsWithDeprecations counts validated documents that used at least
	// one deprecated construct, by document type.
	DocsWithDeprecations *prometheus.CounterVec
	// PermissionLookups counts document permission lookups against the
	// repository by outcome.
	PermissionLookups *prometheus.CounterVec
	// NotificationsWritten counts feed rows written by the family of the
	// event.
	NotificationsWritten *prometheus.CounterVec
	// CleanerDeletions counts rows the cleaner deleted by item.
	CleanerDeletions *prometheus.CounterVec
	// StreamNotificationsSent counts rows sent on notification streams
	// by reason.
	StreamNotificationsSent *prometheus.CounterVec
	// StreamKeepalives counts keepalives sent on notification streams.
	StreamKeepalives prometheus.Counter
	// StreamWakeupsDropped counts notification wakeups a stream had no
	// room for.
	StreamWakeupsDropped prometheus.Counter
}

// NewMetrics registers the service metrics with reg and fails if any
// registration clashes.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	h := elephantine.NewMetricsHelper(reg)

	var m Metrics

	h.CounterVec(&m.Deprecations, prometheus.CounterOpts{
		Name: "elephant_user_deprecations_total",
		Help: "Uses of deprecated schema constructs by deprecation label. " +
			"A label that stops counting can have its deprecation enforced.",
	}, []string{LabelDeprecation})

	h.CounterVec(&m.DocsWithDeprecations, prometheus.CounterOpts{
		Name: "elephant_user_docs_with_deprecations_total",
		Help: "Validated documents that used at least one deprecated " +
			"construct, by document type.",
	}, []string{LabelDocType})

	h.CounterVec(&m.PermissionLookups, prometheus.CounterOpts{
		Name: "elephant_user_permission_lookups_total",
		Help: "Document permission lookups by outcome: hit from the cache, " +
			"miss answered by the repository, denied for a token the " +
			"repository refused, not_found, or error. A rising error " +
			"rate means document access checks are failing.",
	}, []string{LabelOutcome})

	h.CounterVec(&m.NotificationsWritten, prometheus.CounterOpts{
		Name: "elephant_user_notifications_written_total",
		Help: "Notification feed rows written, one per event and key, by " +
			"the family the event came from. A family that stops " +
			"counting has stopped producing notifications; a jump in " +
			"ingest is a producer or a subscription fan-out growing.",
	}, []string{LabelFamily})

	h.CounterVec(&m.CleanerDeletions, prometheus.CounterOpts{
		Name: "elephant_user_cleaner_deletions_total",
		Help: "Rows the retention cleaner deleted, by item: subscription " +
			"for expired notification subscriptions, notification for " +
			"feed rows past the retention. Flat while the feed is in use " +
			"means the cleaner is not running.",
	}, []string{LabelItem})

	h.CounterVec(&m.StreamNotificationsSent, prometheus.CounterOpts{
		Name: "elephant_user_stream_notifications_sent_total",
		Help: "Notification rows sent on streams, by reason: replay for " +
			"rows after the cursor when a stream opens, live for rows " +
			"sent as they are written. Live flat while ingest counts " +
			"means streams are not being woken; replay jumping after a " +
			"deploy is every client catching up.",
	}, []string{LabelReason})

	h.Counter(&m.StreamKeepalives, prometheus.CounterOpts{
		Name: "elephant_user_stream_keepalives_total",
		Help: "Keepalives sent on quiet notification streams. Roughly open " +
			"streams times three a minute when the feed is quiet; zero " +
			"with streams open means quiet streams are left to the " +
			"ingress idle timeout.",
	})

	h.Counter(&m.StreamWakeupsDropped, prometheus.CounterOpts{
		Name: "elephant_user_stream_wakeups_dropped_total",
		Help: "Notification wakeups a stream had no room for, each " +
			"followed by a re-read of the stream's keys. Anything but " +
			"rare means streams cannot keep up with the feed and are " +
			"reading their keys again under load.",
	})

	err := h.Err()
	if err != nil {
		return nil, fmt.Errorf("register service metrics: %w", err)
	}

	return &m, nil
}
