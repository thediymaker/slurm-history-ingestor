package ingestor

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestResolveSyncStart(t *testing.T) {
	phoenix := time.FixedZone("MST", -7*3600)
	cursor := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	lastEnd := time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC)
	initial := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := func(tm time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: tm, Valid: true} }
	none := pgtype.Timestamptz{}

	tests := []struct {
		name       string
		cursor     pgtype.Timestamptz
		lastJobEnd pgtype.Timestamptz
		lookback   time.Duration
		want       time.Time
		wantSource string
	}{
		{
			// The quiet-cluster case: cursor is far ahead of the last job's end,
			// and must win so the pass does not re-scan from lastEnd.
			name:       "cursor wins over job history",
			cursor:     ts(cursor),
			lastJobEnd: ts(lastEnd),
			lookback:   5 * time.Minute,
			want:       cursor.Add(-5 * time.Minute),
			wantSource: startFromCursor,
		},
		{
			name:       "cursor without job history",
			cursor:     ts(cursor),
			lookback:   0,
			want:       cursor,
			wantSource: startFromCursor,
		},
		{
			name:       "only job history (upgrade from pre-cursor install)",
			cursor:     none,
			lastJobEnd: ts(lastEnd),
			lookback:   15 * time.Minute,
			want:       lastEnd.Add(-15 * time.Minute),
			wantSource: startFromLastJobEnd,
		},
		{
			name:       "neither: initial sync date, no lookback",
			lookback:   15 * time.Minute,
			want:       initial,
			wantSource: startFromInitialDate,
		},
		{
			name:       "non-UTC cursor normalized to UTC",
			cursor:     ts(cursor.In(phoenix)),
			lookback:   time.Hour,
			want:       cursor.Add(-time.Hour),
			wantSource: startFromCursor,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source := resolveSyncStart(tt.cursor, tt.lastJobEnd, initial, tt.lookback)
			if !got.Equal(tt.want) {
				t.Errorf("start = %s, want %s", got.Format(time.RFC3339), tt.want.Format(time.RFC3339))
			}
			if got.Location() != time.UTC {
				t.Errorf("start location = %s, want UTC", got.Location())
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}
