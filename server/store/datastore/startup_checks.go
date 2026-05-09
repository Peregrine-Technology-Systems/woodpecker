package datastore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/store"
)

// RunStartupChecks runs one-time post-migration validation after DB init (#64).
// No-op for non-datastore store implementations.
func RunStartupChecks(_ context.Context, s store.Store) {
	st, ok := s.(*storage)
	if !ok {
		return
	}
	scanCorruptJSON(st)
}

// scanCorruptJSON scans all pipeline JSON columns for invalid JSON (#64).
// Logs WARN per corrupt (pipeline_id, column) pair and a summary count.
// Read-only — does not modify any rows.
func scanCorruptJSON(st *storage) {
	const query = `SELECT id, repo_id, errors, cancel_info, event_reason, additional_variables, pr_labels FROM pipelines`
	rows, err := st.engine.QueryString(query)
	if err != nil {
		log.Warn().Err(err).Msg("startup: corrupt JSON scan query failed (#64)")
		return
	}

	jsonCols := []string{"errors", "cancel_info", "event_reason", "additional_variables", "pr_labels"}
	corrupt := 0
	for _, row := range rows {
		for _, col := range jsonCols {
			val := row[col]
			if val == "" || val == "null" {
				continue
			}
			if !json.Valid([]byte(val)) {
				h := fmt.Sprintf("%x", sha256.Sum256([]byte(val)))[:12]
				log.Warn().
					Str("pipeline_id", row["id"]).
					Str("repo_id", row["repo_id"]).
					Str("column", col).
					Str("sha256", h).
					Msg("startup: corrupt JSON in pipeline column — NULL this row to restore normal listing (#64)")
				corrupt++
			}
		}
	}
	log.Info().Int("corrupt", corrupt).Int("scanned", len(rows)).Msg("startup: pipeline JSON scan complete (#64)")
}
