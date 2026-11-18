-- name: RecordRepository :exec
INSERT INTO faucet_repos (repo_id, owner_id, owner_login, full_name, seen_at)
VALUES (@repo_id, @owner_id, @owner_login, @full_name, @seen_at)
ON CONFLICT (repo_id) DO NOTHING;

-- name: RenameRepository :exec
INSERT INTO faucet_repos (repo_id, owner_id, owner_login, full_name, seen_at)
VALUES (@repo_id, @owner_id, @owner_login, @full_name, @seen_at)
ON CONFLICT (repo_id) DO UPDATE
SET owner_id = excluded.owner_id, owner_login = excluded.owner_login,
    full_name = excluded.full_name, seen_at = excluded.seen_at
WHERE lower(faucet_repos.full_name) = lower(@previous_full_name::text);

-- name: GetRepositoryByName :one
-- The owner part of the name is the login of the account that holds it now,
-- and the name part is the one that the row was first seen or renamed under.
SELECT r.repo_id, r.owner_id, o.login AS owner_login, (o.login || '/' || split_part(r.full_name, '/', 2))::text AS full_name
FROM faucet_owners o
JOIN faucet_repos r ON r.owner_id = o.owner_id
WHERE lower(o.login) = lower(@owner_login::text) AND lower(split_part(r.full_name, '/', 2)) = lower(@name::text)
ORDER BY r.seen_at DESC, r.repo_id DESC
LIMIT 1;

-- name: ReleaseOwnerLogin :exec
-- Another account that held the login gives it up, because one login names
-- one account.
DELETE FROM faucet_owners WHERE lower(login) = lower(@login::text) AND owner_id <> @owner_id;

-- name: RecordOwner :exec
INSERT INTO faucet_owners (owner_id, login, seen_at)
VALUES (@owner_id, @login, @seen_at)
ON CONFLICT (owner_id) DO UPDATE SET login = excluded.login, seen_at = excluded.seen_at;

-- name: AddOwner :exec
-- An account that holds a row, or a login that another account holds, makes
-- it write nothing.
INSERT INTO faucet_owners (owner_id, login, seen_at)
VALUES (@owner_id, @login, @seen_at)
ON CONFLICT DO NOTHING;

-- name: GetOwnerByLogin :one
SELECT owner_id, login
FROM faucet_owners
WHERE lower(login) = lower(@login::text);
