-- An org grant names the account that held its login when the client was
-- made, as org:<login>#<account ID>, so it follows the account through a
-- rename and no later holder of the login gets it. A grant made before named
-- only a login, which faucet resolved at each request. This binds it to the
-- account that holds the login now, which is the one that it reads at this
-- moment, under that account's login. A grant whose login no account holds
-- keeps its form, which faucet reads as no grant.
UPDATE faucet_service_clients c
SET grants = ARRAY(
  SELECT coalesce(
    (SELECT 'org:' || o.login || '#' || o.owner_id
     FROM faucet_owners o
     WHERE g LIKE 'org:%' AND position('#' IN g) = 0 AND lower(o.login) = lower(substr(g, 5))),
    g)
  FROM unnest(c.grants) WITH ORDINALITY AS u(g, n)
  ORDER BY n
)
WHERE EXISTS (SELECT 1 FROM unnest(c.grants) AS g WHERE g LIKE 'org:%' AND position('#' IN g) = 0);
