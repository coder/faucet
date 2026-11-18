-- The build before has no trust rules and needs a secret hash for every
-- client. A client with no secret gets the hash of a random value that no one
-- holds, so it still gets no token.
DROP TABLE faucet_workload_trusts;
UPDATE faucet_service_clients SET secret_hash = sha256(uuid_send(gen_random_uuid())) WHERE secret_hash IS NULL;
ALTER TABLE faucet_service_clients ALTER COLUMN secret_hash SET NOT NULL;
