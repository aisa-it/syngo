BEGIN;
-- отклоненное событие (как после проверки auth при федерации)
INSERT INTO rejections (event_id, reason, last_check) VALUES ('$8-0VcodTDq14QA2Q5pG6jgeiDZ35ROQipjEmFjQltTo', 'auth_error', '1');
UPDATE events SET rejection_reason = 'auth_error' WHERE event_id = '$8-0VcodTDq14QA2Q5pG6jgeiDZ35ROQipjEmFjQltTo';
-- soft-failed
UPDATE event_json SET internal_metadata = '{"soft_failed":true}' WHERE event_id = '$o7Us-GMFP9A9ama0IP8BkeUqUSpE9UjLTWt__hI7RSM';
-- outlier
UPDATE events SET outlier = true WHERE event_id = '$MsaQGHussQvySRY4xm7ejFpNUiavuuAoouQNIVWkUMc';
-- backfill: отрицательный stream_ordering
UPDATE events SET stream_ordering = -1 WHERE event_id = '$EoPAqPLTfFS4vdmn3XSXOsIvrQPtXM3Mc8H61lu3vJw';
-- форк, разрешенный state resolution: текущей темой осталась старая
UPDATE current_state_events SET event_id = '$X3veAaF4eYkebVmz4aOuhSEZoq8AABHn-7ZovATVdjQ',
  event_stream_ordering = (SELECT stream_ordering FROM events WHERE event_id = '$X3veAaF4eYkebVmz4aOuhSEZoq8AABHn-7ZovATVdjQ')
WHERE room_id = '!JqfZItlTHtfMcShmLM:c4.test' AND type = 'm.room.topic' AND state_key = '';
COMMIT;
