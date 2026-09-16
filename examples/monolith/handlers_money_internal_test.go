package monolith

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProviderEvent_MomentInUTC — момент из тела вебхука приводится к UTC там,
// где разобран; событие без occurred_at получает момент приёма.
func TestProviderEvent_MomentInUTC(t *testing.T) {
	at := time.Date(2031, time.March, 8, 23, 0, 0, 0, time.FixedZone("UTC-03:30", -(3*60*60+30*60)))
	received := time.Date(2031, time.March, 9, 2, 30, 15, 0, time.UTC)

	got := providerEvent{At: at}.event(received).OccurredAt
	require.Same(t, time.UTC, got.Location(), "момент события в поясе %s", got.Location())
	require.True(t, got.Equal(at), "то же мгновение: %s", got)

	require.True(t, providerEvent{}.event(received).OccurredAt.Equal(received), "без occurred_at — момент приёма")
}
