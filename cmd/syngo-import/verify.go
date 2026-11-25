package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/aisa-it/syngo/internal/store"
)

// verifyReport - сверка с источником: для каждого пользователя из
// user_stats дампа число строк каждого вида в SYNGO должно совпасть с Synapse.
type verifyReport struct {
	Users      int              `json:"users"`
	Checked    map[string]int   `json:"checked"`
	Mismatches []verifyMismatch `json:"mismatches"`
}

// verifyMismatch - расхождение по одному виду данных пользователя.
type verifyMismatch struct {
	UserID string `json:"user_id"`
	What   string `json:"what"`
	Source int    `json:"source"`
	Target int    `json:"target"`
}

// verifyQueries - счетчики SYNGO, сопоставимые с user_stats из export.sql.
// one_time_keys - только невыданные: выданные Synapse удаляет.
//
//nolint:gosec // SQL-запросы счетчиков, не учетные данные
var verifyQueries = map[string]string{
	"devices": `SELECT count(*) FROM devices WHERE user_id = $1 AND device_id <> 'ADMIN'`,
	"access_tokens": `SELECT count(*) FROM access_tokens
		WHERE user_id = $1 AND revoked_at IS NULL AND scope = ''`,
	"device_keys":        `SELECT count(*) FROM device_keys WHERE user_id = $1`,
	"one_time_keys":      `SELECT count(*) FROM one_time_keys WHERE user_id = $1 AND claimed_at IS NULL`,
	"fallback_keys":      `SELECT count(*) FROM e2e_fallback_keys WHERE user_id = $1`,
	"cross_signing_keys": `SELECT count(*) FROM e2e_cross_signing_keys WHERE user_id = $1`,
	"signatures":         `SELECT count(*) FROM e2e_signatures WHERE signer_user_id = $1`,
	"room_keys_versions": `SELECT count(*) FROM room_keys_versions WHERE user_id = $1`,
	"room_keys":          `SELECT count(*) FROM room_keys WHERE user_id = $1`,
	"account_data":       `SELECT count(*) FROM account_data WHERE user_id = $1`,
	"push_rules":         `SELECT count(*) FROM push_rules WHERE user_id = $1`,
	"pushers":            `SELECT count(*) FROM pushers WHERE user_id = $1`,
	"filters":            `SELECT count(*) FROM user_filters WHERE user_id = $1`,
	"to_device":          `SELECT count(*) FROM device_inbox WHERE user_id = $1`,
	"external_ids":       `SELECT count(*) FROM user_external_ids WHERE user_id = $1`,
	"receipts": `SELECT count(*) FROM receipts
		WHERE user_id = $1 AND receipt_type IN ('m.read', 'm.read.private')`,
}

// verifyUsers сверяет счетчики по пользователям с user_stats источника. Пользователь,
// которого нет в SYNGO, - тоже расхождение.
func verifyUsers(ctx context.Context, db *store.DB, records []dumpRecord) (*verifyReport, error) {
	report := &verifyReport{Checked: map[string]int{}}

	kinds := make([]string, 0, len(verifyQueries))
	for kind := range verifyQueries {
		kinds = append(kinds, kind)
	}

	slices.Sort(kinds)

	for i := range records {
		record := &records[i]
		if record.Kind != "user_stats" {
			continue
		}

		report.Users++

		var exists bool
		if err := db.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE user_id = $1)`,
			record.UserID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("сверка %s: %w", record.UserID, err)
		}

		if !exists {
			report.Mismatches = append(report.Mismatches, verifyMismatch{UserID: record.UserID, What: "user", Source: 1})

			continue
		}

		for _, kind := range kinds {
			source, ok := record.Stats[kind]
			if !ok {
				continue
			}

			var target int
			if err := db.Pool().QueryRow(ctx, verifyQueries[kind], record.UserID).Scan(&target); err != nil {
				return nil, fmt.Errorf("сверка %s/%s: %w", record.UserID, kind, err)
			}

			report.Checked[kind] += source

			if target != source {
				report.Mismatches = append(report.Mismatches, verifyMismatch{
					UserID: record.UserID, What: kind, Source: source, Target: target,
				})
			}
		}
	}

	return report, nil
}

// print выводит итог сверки.
func (r *verifyReport) print() {
	fmt.Printf("сверка: пользователей %d, расхождений %d\n", r.Users, len(r.Mismatches))

	for _, m := range r.Mismatches {
		fmt.Printf("  %s %s: источник %d, SYNGO %d\n", m.UserID, m.What, m.Source, m.Target)
	}
}
