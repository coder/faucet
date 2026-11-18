-- The build before has no trust rules and needs a secret hash for every
-- client. A client with no secret gets the hash of 32 zero bytes, which no
-- known secret has, so it still gets no token, and the up turns that hash
-- back into no secret. The lock keeps a client with no secret from coming in
-- between the update and SET NOT NULL.
LOCK TABLE faucet_service_clients IN EXCLUSIVE MODE;
DROP TABLE faucet_workload_trusts;
UPDATE faucet_service_clients SET secret_hash = decode(repeat('00', 32), 'hex') WHERE secret_hash IS NULL;
ALTER TABLE faucet_service_clients ALTER COLUMN secret_hash SET NOT NULL;
