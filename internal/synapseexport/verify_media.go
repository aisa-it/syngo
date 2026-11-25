package synapseexport

import (
	"context"
	"fmt"
	"strings"
)

// Сверка медиа, пользователей и алиасов: раньше сверка
// касалась только событий, и потеря медиа, карантина или пользователей была
// видна лишь по отчету импорта.

// Медиа Synapse без превью ссылок (url_cache - кеш, не переносится):
// размер, карантин, sha256 (колонки нет в старых Synapse - через to_jsonb).
const sourceMedia = `
	SELECT m.media_id,
	       coalesce(m.media_length, 0)::text || '|' || (m.quarantined_by IS NOT NULL)::text || '|' ||
	       coalesce(to_jsonb(m) ->> 'sha256', '')
	FROM local_media_repository m
	WHERE m.url_cache IS NULL
	ORDER BY m.media_id COLLATE "C"`

const targetMedia = `
	SELECT media_id,
	       size_bytes::text || '|' || (blocked AND quarantined_by IS NOT NULL)::text || '|' ||
	       coalesce(encode(sha256, 'hex'), '')
	FROM media
	WHERE NOT is_remote AND ($1 = '' OR server_name = $1)
	ORDER BY media_id COLLATE "C"`

// verifyMedia сверяет медиа: набор media_id, размер, карантин и sha256 (если
// Synapse его считал). Пустой sha256 у нас - байты не перенесены (missing).
func (r *VerifyReport) verifyMedia(ctx context.Context, src, dst Querier, opts VerifyOptions) error {
	var exists bool
	if err := src.QueryRow(ctx, `SELECT to_regclass('local_media_repository') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("synapseexport: медиа Synapse: %w", err)
	}

	if !exists {
		return nil
	}

	r.MediaChecked = true

	if err := src.QueryRow(ctx, `SELECT count(*) FROM local_media_repository WHERE url_cache IS NOT NULL`).
		Scan(&r.MediaURLCache); err != nil {
		return fmt.Errorf("synapseexport: url_cache Synapse: %w", err)
	}

	diffs := samples{limit: opts.MaxDiffs}

	err := mergeKeyed(ctx, src, dst, query{sql: sourceMedia}, query{sql: targetMedia, args: []any{opts.ServerName}},
		func(key, srcVal, dstVal string) {
			if srcVal != absent {
				r.Media++
			}

			if msg := mediaDiff(srcVal, dstVal); msg != "" {
				r.MediaDiffs++
				diffs.add(key + ": " + msg)
			}
		})
	if err != nil {
		return err
	}

	if r.MediaDiffs > 0 {
		r.problem("медиа расходится у %d записей: %s", r.MediaDiffs, diffs.String())
	}

	return nil
}

// mediaDiff описывает расхождение медиа ("" - совпадает).
func mediaDiff(srcVal, dstVal string) string {
	switch {
	case srcVal == absent:
		return "нет в Synapse"
	case dstVal == absent:
		return "нет у нас"
	}

	s, d := strings.SplitN(srcVal, "|", 3), strings.SplitN(dstVal, "|", 3)
	if len(s) != 3 || len(d) != 3 {
		return "synapse=" + srcVal + " syngo=" + dstVal
	}

	var problems []string

	if s[0] != d[0] {
		problems = append(problems, "размер synapse="+s[0]+" syngo="+d[0])
	}

	if s[1] != d[1] {
		problems = append(problems, "карантин synapse="+s[1]+" syngo="+d[1])
	}

	switch {
	case d[2] == "":
		problems = append(problems, "байты не перенесены (нет sha256)")
	case s[2] != "" && s[2] != d[2]:
		problems = append(problems, "sha256 synapse="+s[2]+" syngo="+d[2])
	}

	return strings.Join(problems, ", ")
}

// verifyUsers сверяет пользователей (user_id, deactivated) и алиасы комнат.
func (r *VerifyReport) verifyUsers(ctx context.Context, src, dst Querier, opts VerifyOptions) error {
	users := samples{limit: opts.MaxDiffs}

	err := mergeKeyed(ctx, src, dst,
		query{sql: `
			SELECT name, (deactivated <> 0)::text FROM users
			WHERE name LIKE '@%' AND ($1 = '' OR name LIKE '%:' || $1)
			ORDER BY name COLLATE "C"`, args: []any{opts.ServerName}},
		query{sql: `
			SELECT user_id, deactivated::text FROM users
			WHERE $1 = '' OR user_id LIKE '%:' || $1
			ORDER BY user_id COLLATE "C"`, args: []any{opts.ServerName}},
		func(key, srcVal, dstVal string) {
			if srcVal != absent {
				r.Users++
			}

			if srcVal != dstVal {
				r.UserDiffs++
				users.add(fmt.Sprintf("%s: synapse deactivated=%s syngo=%s", key, orDash(srcVal), orDash(dstVal)))
			}
		})
	if err != nil {
		return err
	}

	if r.UserDiffs > 0 {
		r.problem("пользователи расходятся у %d: %s", r.UserDiffs, users.String())
	}

	aliases := samples{limit: opts.MaxDiffs}

	err = mergeKeyed(ctx, src, dst,
		query{sql: `SELECT room_alias, room_id FROM room_aliases ORDER BY room_alias COLLATE "C"`},
		query{sql: `SELECT room_alias, room_id FROM room_aliases ORDER BY room_alias COLLATE "C"`},
		func(key, srcVal, dstVal string) {
			if srcVal != absent {
				r.Aliases++
			}

			if srcVal != dstVal {
				r.AliasDiffs++
				aliases.add(fmt.Sprintf("%s: synapse=%s syngo=%s", key, orDash(srcVal), orDash(dstVal)))
			}
		})
	if err != nil {
		return err
	}

	if r.AliasDiffs > 0 {
		r.problem("алиасы расходятся у %d: %s", r.AliasDiffs, aliases.String())
	}

	return nil
}
