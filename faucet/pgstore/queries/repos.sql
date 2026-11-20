-- name: RecordRepository :exec
INSERT INTO faucet_repos (repo_id, owner_id, owner_login, full_name, seen_at)
VALUES (@repo_id, @owner_id, @owner_login, @full_name, @seen_at)
ON CONFLICT (repo_id) DO NOTHING;

-- name: RecordRepositoryRename :execrows
-- A body that the store judged before writes nothing.
INSERT INTO faucet_repo_renames (body_sha256, repo_id, seen_at)
VALUES (@body_sha256, @repo_id, @seen_at)
ON CONFLICT (body_sha256) DO NOTHING;

-- name: LockRepositoryHolding :one
-- holds_from says whether the row's full name is @from_full_name, as the row
-- holds it or under the login that the row's owner holds now. GitHub writes
-- the name from under the owner's login now, which the row lacks when the
-- owner was renamed after the row was first seen.
SELECT (lower(r.full_name) = lower(@from_full_name::text) OR EXISTS (
     SELECT 1 FROM faucet_owners o
     WHERE o.owner_id = r.owner_id
       AND lower(o.login || '/' || split_part(r.full_name, '/', 2)) = lower(@from_full_name::text)
   ))::boolean AS holds_from,
   (r.quarantined_at IS NOT NULL)::boolean AS quarantined
FROM faucet_repos r
WHERE r.repo_id = @repo_id
FOR UPDATE OF r;

-- name: RenameRepository :exec
INSERT INTO faucet_repos (repo_id, owner_id, owner_login, full_name, seen_at)
VALUES (@repo_id, @owner_id, @owner_login, @full_name, @seen_at)
ON CONFLICT (repo_id) DO UPDATE
SET owner_id = excluded.owner_id, owner_login = excluded.owner_login,
    full_name = excluded.full_name, seen_at = excluded.seen_at,
    quarantined_at = NULL;

-- name: QuarantineRepository :exec
UPDATE faucet_repos
SET quarantined_at = COALESCE(quarantined_at, @seen_at)
WHERE repo_id = @repo_id;

-- name: GetRepositoryByName :one
-- The owner part of the name is the login of the account that holds it now,
-- and the name part is the one that the row was first seen or renamed under.
-- A quarantined row resolves no name, so the caller refuses it rather than
-- take an older row that held the name.
SELECT r.repo_id, r.owner_id, o.login AS owner_login, (o.login || '/' || split_part(r.full_name, '/', 2))::text AS full_name,
       (r.quarantined_at IS NOT NULL)::boolean AS quarantined
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
INSERT INTO faucet_owners (owner_id, login)
VALUES (@owner_id, @login)
ON CONFLICT (owner_id) DO UPDATE SET login = excluded.login;

-- name: AddOwner :exec
-- An account that holds a row, or a login that another account holds, makes
-- it write nothing.
INSERT INTO faucet_owners (owner_id, login)
VALUES (@owner_id, @login)
ON CONFLICT DO NOTHING;

-- name: GetOwnerByID :one
SELECT owner_id, login
FROM faucet_owners
WHERE owner_id = @owner_id;

-- name: GetOwnerByLogin :one
SELECT owner_id, login
FROM faucet_owners
WHERE lower(login) = lower(@login::text);
