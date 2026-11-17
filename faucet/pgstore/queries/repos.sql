-- name: UpsertRepository :exec
INSERT INTO faucet_repos (repo_id, owner_id, owner_login, full_name, seen_at)
VALUES (@repo_id, @owner_id, @owner_login, @full_name, @seen_at)
ON CONFLICT (repo_id) DO UPDATE
SET owner_id = excluded.owner_id, owner_login = excluded.owner_login,
    full_name = excluded.full_name, seen_at = excluded.seen_at;

-- name: GetRepositoryByName :one
SELECT repo_id, owner_id, owner_login, full_name
FROM faucet_repos
WHERE lower(full_name) = lower(@full_name::text)
ORDER BY seen_at DESC, repo_id DESC
LIMIT 1;
