-- event_types holds the stored event types that the event name of the
-- subscription matches, resolved at every subscribe. Each name before this
-- migration is one stored type, so it matches that type.
ALTER TABLE faucet_subscriptions ADD COLUMN event_types text[];
UPDATE faucet_subscriptions SET event_types = ARRAY[event_type];
ALTER TABLE faucet_subscriptions
  ALTER COLUMN event_types SET NOT NULL,
  ADD CONSTRAINT faucet_subscriptions_event_types_check CHECK (cardinality(event_types) > 0);

-- During a rolling deploy an instance of the build before this migration
-- inserts rows without event_types. Every name it accepts is one stored type.
CREATE FUNCTION faucet_subscriptions_default_event_types() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.event_types IS NULL THEN
    NEW.event_types := ARRAY[NEW.event_type];
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER faucet_subscriptions_default_event_types
  BEFORE INSERT ON faucet_subscriptions
  FOR EACH ROW EXECUTE FUNCTION faucet_subscriptions_default_event_types();
