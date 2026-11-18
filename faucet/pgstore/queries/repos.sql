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
SELECT repo_id, owner_id, owner_login, full_name
FROM faucet_repos
WHERE lower(full_name) = lower(@full_name::text)
ORDER BY seen_at DESC, repo_id DESC
LIMIT 1;

-- name: GetOwnerByLogin :one
SELECT owner_id, owner_login
FROM faucet_repos
WHERE lower(owner_login) = lower(@owner_login::text)
ORDER BY seen_at DESC, repo_id DESC
LIMIT 1;
