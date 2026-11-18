-- An org grant names a login again, the one that its account held when the
-- grant was bound, and the build before resolves it at each request.
UPDATE faucet_service_clients c
SET grants = ARRAY(
  SELECT regexp_replace(g, '^(org:[^#]+)#[0-9]+$', '\1')
  FROM unnest(c.grants) WITH ORDINALITY AS u(g, n)
  ORDER BY n
)
WHERE EXISTS (SELECT 1 FROM unnest(c.grants) AS g WHERE g ~ '^org:[^#]+#[0-9]+$');
