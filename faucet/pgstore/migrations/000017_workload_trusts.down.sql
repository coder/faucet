-- The build before has no trust rules and needs a secret hash for every
-- client. A client with no secret gets an all-zero secret_hash, which no
-- known secret hashes to, so it still gets no token, and the up turns it back
-- into no secret. The lock keeps a client with no secret from coming in
-- between the update and SET NOT NULL.
LOCK TABLE faucet_service_clients IN EXCLUSIVE MODE;
DROP TABLE faucet_workload_trusts;
UPDATE faucet_service_clients SET secret_hash = decode(repeat('00', 32), 'hex') WHERE secret_hash IS NULL;
ALTER TABLE faucet_service_clients ALTER COLUMN secret_hash SET NOT NULL;
