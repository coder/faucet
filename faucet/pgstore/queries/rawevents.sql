-- name: RawInterests :many
SELECT
  false AS is_lease, s.key_hash AS id, s.created_at AS incarnation,
  (CASE WHEN s.arguments ? 'fields' THEN ARRAY(SELECT jsonb_array_elements_text(s.arguments -> 'fields')) END)::text[] AS fields,
  (SELECT COUNT(*) FROM faucet_raw_deliveries d WHERE d.key_hash = s.key_hash AND d.incarnation = s.created_at)::int AS pending
FROM faucet_subscriptions s
WHERE s.repo_id = sqlc.arg(repo_id)
  AND s.event_type = 'github.' || sqlc.arg(event)::text
  AND s.expires_at > sqlc.arg(now)
  AND (NOT (s.arguments ? 'actions') OR (s.arguments -> 'actions') ? sqlc.arg(action)::text)
UNION ALL
SELECT
  true, l.id, l.created_at, l.fields,
  (SELECT COUNT(*) FROM faucet_raw_unread u WHERE u.lease_id = l.id AND NOT u.lost)::int
FROM faucet_raw_leases l
WHERE l.repo_id = sqlc.arg(repo_id)
  AND l.event = sqlc.arg(event)::text
  AND l.expires_at > sqlc.arg(now)
  AND (cardinality(l.actions) = 0 OR sqlc.arg(action)::text = ANY(l.actions))
ORDER BY is_lease, id;

-- name: InsertRawEvent :one
INSERT INTO faucet_raw_events (delivery_id, event, action, repo_id, body, fields, received_at)
VALUES (
  sqlc.arg(delivery_id),
  sqlc.arg(event),
  sqlc.arg(action),
  sqlc.arg(repo_id),
  CASE WHEN sqlc.arg(has_body)::boolean THEN sqlc.arg(body)::bytea END,
  sqlc.narg(fields)::text[],
  clock_timestamp()
)
ON CONFLICT (delivery_id) DO NOTHING
RETURNING id;

-- name: InsertRawDeliveries :exec
INSERT INTO faucet_raw_deliveries (key_hash, incarnation, raw_event_id, enqueued_at, next_attempt_at)
SELECT k.key_hash, i.incarnation, sqlc.arg(raw_event_id)::bigint, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
FROM unnest(sqlc.arg(key_hashes)::bytea[]) WITH ORDINALITY AS k(key_hash, n)
JOIN unnest(sqlc.arg(incarnations)::timestamptz[]) WITH ORDINALITY AS i(incarnation, n) USING (n);

-- name: InsertRawUnread :exec
INSERT INTO faucet_raw_unread (lease_id, raw_event_id)
SELECT l.lease_id, sqlc.arg(raw_event_id)::bigint
FROM unnest(sqlc.arg(lease_ids)::bytea[]) AS l(lease_id);

-- name: RaiseRawGaps :exec
INSERT INTO faucet_raw_gaps (key_hash, incarnation, through)
SELECT k.key_hash, i.incarnation, sqlc.arg(through)::bigint
FROM unnest(sqlc.arg(key_hashes)::bytea[]) WITH ORDINALITY AS k(key_hash, n)
JOIN unnest(sqlc.arg(incarnations)::timestamptz[]) WITH ORDINALITY AS i(incarnation, n) USING (n)
ORDER BY k.key_hash, i.incarnation
ON CONFLICT (key_hash, incarnation) DO UPDATE SET through = GREATEST(faucet_raw_gaps.through, EXCLUDED.through);

-- name: RaiseRawLost :exec
INSERT INTO faucet_raw_unread (lease_id, raw_event_id, lost)
SELECT l.lease_id, sqlc.arg(raw_event_id)::bigint, true
FROM unnest(sqlc.arg(lease_ids)::bytea[]) AS l(lease_id)
ORDER BY l.lease_id
ON CONFLICT (lease_id) WHERE lost DO UPDATE SET raw_event_id = GREATEST(faucet_raw_unread.raw_event_id, EXCLUDED.raw_event_id);

-- name: RawHead :one
SELECT COALESCE(MAX(id), 0)::bigint AS head FROM faucet_raw_events;

-- name: LockRawLeases :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(principal)::text, 1));

-- name: EndRawLeasesOf :many
WITH live AS (
  SELECT COUNT(*) AS leases FROM faucet_raw_leases v WHERE v.principal = sqlc.arg(principal) AND v.expires_at > sqlc.arg(now)
), expired AS (
  SELECT l.id FROM faucet_raw_leases l
  WHERE l.principal = sqlc.arg(principal) AND l.expires_at <= sqlc.arg(now)
  FOR UPDATE SKIP LOCKED
), oldest AS (
  SELECT l.id FROM faucet_raw_leases l
  WHERE l.principal = sqlc.arg(principal) AND l.expires_at > sqlc.arg(now)
    AND (SELECT leases FROM live) >= sqlc.arg(max_live)::bigint
  ORDER BY l.polled_at, l.id
  LIMIT 1
  FOR UPDATE SKIP LOCKED
)
DELETE FROM faucet_raw_leases l
WHERE l.id IN (SELECT expired.id FROM expired UNION ALL SELECT oldest.id FROM oldest)
RETURNING l.id;

-- name: DeleteRawUnreadOf :exec
DELETE FROM faucet_raw_unread
WHERE (lease_id, raw_event_id) IN (
  SELECT u.lease_id, u.raw_event_id FROM faucet_raw_unread u
  WHERE u.lease_id = ANY(sqlc.arg(lease_ids)::bytea[])
  FOR UPDATE SKIP LOCKED
);

-- name: InsertRawLease :one
INSERT INTO faucet_raw_leases (id, key_hash, principal, event, actions, fields, repo_id, acked_through, created_at, polled_at, expires_at)
VALUES (
  sqlc.arg(id),
  sqlc.arg(key_hash),
  sqlc.arg(principal),
  sqlc.arg(event),
  sqlc.arg(actions)::text[],
  sqlc.narg(fields)::text[],
  sqlc.arg(repo_id),
  (SELECT COALESCE(MAX(e.id), 0) FROM faucet_raw_events e),
  sqlc.arg(now),
  sqlc.arg(now),
  sqlc.arg(expires_at)
)
RETURNING acked_through;

-- name: TouchRawLease :one
WITH found AS (
  SELECT l.id, l.acked_through FROM faucet_raw_leases l
  WHERE l.id = sqlc.arg(id) AND l.key_hash = sqlc.arg(key_hash) AND l.repo_id = sqlc.arg(repo_id) AND l.expires_at > sqlc.arg(now)
  FOR UPDATE
)
UPDATE faucet_raw_leases l
SET acked_through = GREATEST(l.acked_through, sqlc.arg(after)::bigint), polled_at = sqlc.arg(now), expires_at = sqlc.arg(expires_at)
FROM found
WHERE l.id = found.id
RETURNING found.acked_through AS acked_before;

-- name: ReadRawLeasePage :many
-- A row is open while the page before it holds fewer than row_limit events
-- and fewer than max_bytes bytes of bodies: an open row is returned, or passed
-- over when it is lost or its event cannot be served, and the first row that
-- is not open ends the page.
WITH head AS (
  SELECT COALESCE(MAX(id), 0)::bigint AS max_id FROM faucet_raw_events
), unread AS (
  SELECT
    u.raw_event_id,
    COALESCE(NOT u.lost AND e.body IS NOT NULL AND (sqlc.narg(not_before)::timestamptz IS NULL OR e.received_at >= sqlc.narg(not_before)::timestamptz), false) AS served,
    COALESCE(octet_length(e.body), 0)::bigint AS size
  FROM faucet_raw_unread u
  LEFT JOIN faucet_raw_events e ON e.id = u.raw_event_id
  WHERE u.lease_id = sqlc.arg(lease_id) AND u.raw_event_id > sqlc.arg(after)
), ranked AS (
  SELECT
    raw_event_id, served,
    (COUNT(*) FILTER (WHERE served) OVER earlier < sqlc.arg(row_limit)::bigint
      AND COALESCE(SUM(size) FILTER (WHERE served) OVER earlier, 0) < sqlc.arg(max_bytes)::bigint) AS open
  FROM unread
  WINDOW earlier AS (ORDER BY raw_event_id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING)
), summary AS (
  SELECT
    (SELECT max_id FROM head)::bigint AS head,
    (COUNT(*) FILTER (WHERE open AND NOT served))::bigint AS skipped,
    COALESCE(MAX(raw_event_id) FILTER (WHERE open), 0)::bigint AS last_open,
    COALESCE(bool_or(NOT open), false)::boolean AS more
  FROM ranked
)
SELECT
  summary.head, summary.skipped, summary.last_open, summary.more,
  e.id, e.delivery_id, e.event, e.action, e.repo_id, e.body, e.fields, e.received_at
FROM summary
LEFT JOIN ranked r ON r.open AND r.served
LEFT JOIN faucet_raw_events e ON e.id = r.raw_event_id
ORDER BY e.id;

-- name: AckRawUnread :exec
DELETE FROM faucet_raw_unread WHERE lease_id = sqlc.arg(lease_id) AND raw_event_id <= sqlc.arg(after);

-- name: ClaimRawDelivery :one
-- The origin expression is the one of ClaimDelivery.
WITH due AS (
  SELECT d.key_hash, d.raw_event_id
  FROM faucet_raw_deliveries d
  JOIN faucet_subscriptions s ON s.key_hash = d.key_hash AND s.created_at = d.incarnation
  WHERE s.expires_at > sqlc.arg(now)
    AND d.next_attempt_at <= sqlc.arg(now)
    AND (d.lease_until IS NULL OR d.lease_until <= sqlc.arg(now))
    AND lower(COALESCE(regexp_replace(substring(s.callback_url from '^[A-Za-z][A-Za-z0-9+.-]*://[^/?#]*'), '^([^/]*//).*@', '\1'), '')) <> ALL(sqlc.arg(skip_origins)::text[])
  ORDER BY d.next_attempt_at, d.raw_event_id
  LIMIT 1
  FOR UPDATE OF d SKIP LOCKED
), claimed AS (
  UPDATE faucet_raw_deliveries d
  SET attempts = d.attempts + 1, lease_until = sqlc.arg(lease_until)
  FROM due
  WHERE d.key_hash = due.key_hash AND d.raw_event_id = due.raw_event_id
  RETURNING d.key_hash, d.incarnation, d.raw_event_id, d.attempts, d.enqueued_at
)
SELECT
  c.key_hash, c.incarnation, c.raw_event_id, c.attempts, c.enqueued_at,
  s.event_type, s.arguments, s.callback_url, s.secret_enc, s.previous_secret_enc, s.previous_secret_until, s.repo_id,
  (SELECT COALESCE(MIN(p.raw_event_id), c.raw_event_id) - 1 FROM faucet_raw_deliveries p WHERE p.key_hash = c.key_hash AND p.incarnation = c.incarnation)::bigint AS watermark,
  e.id AS stored_event_id, e.delivery_id, e.event, e.action, e.repo_id AS event_repo_id, e.body, e.fields, e.received_at
FROM claimed c
JOIN faucet_subscriptions s ON s.key_hash = c.key_hash AND s.created_at = c.incarnation
LEFT JOIN faucet_raw_events e ON e.id = c.raw_event_id AND e.body IS NOT NULL;

-- name: RescheduleRawDelivery :execrows
UPDATE faucet_raw_deliveries
SET next_attempt_at = sqlc.arg(next_attempt_at), lease_until = NULL
WHERE key_hash = sqlc.arg(key_hash) AND raw_event_id = sqlc.arg(raw_event_id) AND attempts = sqlc.arg(attempt);

-- name: EndRawDelivery :execrows
DELETE FROM faucet_raw_deliveries
WHERE key_hash = sqlc.arg(key_hash) AND raw_event_id = sqlc.arg(raw_event_id) AND attempts = sqlc.arg(attempt);

-- name: DeleteRawDeliveriesOf :execrows
DELETE FROM faucet_raw_deliveries WHERE key_hash = sqlc.arg(key_hash) AND incarnation = sqlc.arg(incarnation);

-- name: ClaimRawGap :one
WITH due AS (
  SELECT g.key_hash, g.incarnation
  FROM faucet_raw_gaps g
  JOIN faucet_subscriptions s ON s.key_hash = g.key_hash AND s.created_at = g.incarnation
  WHERE s.expires_at > sqlc.arg(now)
    AND (g.lease_until IS NULL OR g.lease_until <= sqlc.arg(now))
    AND lower(COALESCE(regexp_replace(substring(s.callback_url from '^[A-Za-z][A-Za-z0-9+.-]*://[^/?#]*'), '^([^/]*//).*@', '\1'), '')) <> ALL(sqlc.arg(skip_origins)::text[])
  ORDER BY g.through, g.key_hash
  LIMIT 1
  FOR UPDATE OF g SKIP LOCKED
), claimed AS (
  UPDATE faucet_raw_gaps g
  SET lease_until = sqlc.arg(lease_until)
  FROM due
  WHERE g.key_hash = due.key_hash AND g.incarnation = due.incarnation
  RETURNING g.key_hash, g.incarnation, g.through
)
SELECT c.key_hash, c.incarnation, c.through, s.callback_url, s.secret_enc, s.previous_secret_enc, s.previous_secret_until
FROM claimed c
JOIN faucet_subscriptions s ON s.key_hash = c.key_hash AND s.created_at = c.incarnation;

-- name: EndRawGap :exec
DELETE FROM faucet_raw_gaps
WHERE key_hash = sqlc.arg(key_hash) AND incarnation = sqlc.arg(incarnation) AND through <= sqlc.arg(through);

-- name: ReleaseRawGap :exec
UPDATE faucet_raw_gaps SET lease_until = NULL WHERE key_hash = sqlc.arg(key_hash) AND incarnation = sqlc.arg(incarnation);

-- name: ExpireRawDeliveries :execrows
DELETE FROM faucet_raw_deliveries
WHERE (key_hash, raw_event_id) IN (
  SELECT d.key_hash, d.raw_event_id
  FROM faucet_raw_deliveries d
  WHERE (d.lease_until IS NULL OR d.lease_until <= sqlc.arg(now)::timestamptz)
    AND NOT EXISTS (
      SELECT 1 FROM faucet_subscriptions s
      WHERE s.key_hash = d.key_hash AND s.created_at = d.incarnation AND s.expires_at > sqlc.arg(now)
    )
  FOR UPDATE OF d SKIP LOCKED
);

-- name: ExpireRawGaps :exec
DELETE FROM faucet_raw_gaps
WHERE (key_hash, incarnation) IN (
  SELECT g.key_hash, g.incarnation
  FROM faucet_raw_gaps g
  WHERE (g.lease_until IS NULL OR g.lease_until <= sqlc.arg(now)::timestamptz)
    AND NOT EXISTS (
      SELECT 1 FROM faucet_subscriptions s
      WHERE s.key_hash = g.key_hash AND s.created_at = g.incarnation AND s.expires_at > sqlc.arg(now)
    )
  FOR UPDATE OF g SKIP LOCKED
);

-- name: DeleteExpiredRawLeases :execrows
DELETE FROM faucet_raw_leases
WHERE id IN (
  SELECT l.id FROM faucet_raw_leases l WHERE l.expires_at <= sqlc.arg(now) FOR UPDATE SKIP LOCKED
);

-- name: DeleteOrphanedRawUnread :exec
DELETE FROM faucet_raw_unread
WHERE (lease_id, raw_event_id) IN (
  SELECT u.lease_id, u.raw_event_id
  FROM faucet_raw_unread u
  WHERE NOT EXISTS (SELECT 1 FROM faucet_raw_leases l WHERE l.id = u.lease_id)
  FOR UPDATE OF u SKIP LOCKED
);

-- name: ClearRawBodies :execrows
-- A lost row stands for events that the lease missed and needs no body.
UPDATE faucet_raw_events
SET body = NULL, fields = NULL
WHERE id IN (
  SELECT e.id
  FROM faucet_raw_events e
  WHERE e.body IS NOT NULL
    AND (
      e.received_at < sqlc.narg(clear_before)::timestamptz
      OR (
        NOT EXISTS (SELECT 1 FROM faucet_raw_deliveries d WHERE d.raw_event_id = e.id)
        AND NOT EXISTS (SELECT 1 FROM faucet_raw_unread u WHERE u.raw_event_id = e.id AND NOT u.lost)
      )
    )
  FOR UPDATE OF e SKIP LOCKED
);

-- name: DeleteRawEventsBefore :execrows
DELETE FROM faucet_raw_events
WHERE id IN (
  SELECT e.id FROM faucet_raw_events e WHERE e.received_at < sqlc.arg(before) FOR UPDATE SKIP LOCKED
);

-- name: RawWants :many
SELECT
  s.repo_id,
  substring(s.event_type from 8)::text AS event,
  ARRAY(SELECT a FROM jsonb_array_elements_text(COALESCE(s.arguments -> 'actions', '[]'::jsonb)) AS a ORDER BY a COLLATE "C")::text[] AS actions
FROM faucet_subscriptions s
WHERE s.event_type LIKE 'github.%' AND s.expires_at > sqlc.arg(now)
UNION
SELECT l.repo_id, l.event, ARRAY(SELECT a FROM unnest(l.actions) AS a ORDER BY a COLLATE "C")::text[]
FROM faucet_raw_leases l
WHERE l.expires_at > sqlc.arg(now)
ORDER BY 1, 2, 3;
