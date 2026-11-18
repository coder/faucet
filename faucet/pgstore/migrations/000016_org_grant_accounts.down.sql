-- An org grant names a login again: the login that its account holds now, so
-- that the build before, which resolves the login at each request, reads the
-- same account. The login that the grant was bound under can belong to
-- another account by now. A grant whose account holds no login keeps its
-- bound form, which the build before reads as no grant.
UPDATE faucet_service_clients c
SET grants = ARRAY(
  SELECT coalesce(
    (SELECT 'org:' || o.login
     FROM faucet_owners o
     WHERE g ~ '^org:[^#]+#[0-9]+$' AND o.owner_id = substring(g FROM '#([0-9]+)$')::bigint),
    g)
  FROM unnest(c.grants) WITH ORDINALITY AS u(g, n)
  ORDER BY n
)
WHERE EXISTS (SELECT 1 FROM unnest(c.grants) AS g WHERE g ~ '^org:[^#]+#[0-9]+$');
