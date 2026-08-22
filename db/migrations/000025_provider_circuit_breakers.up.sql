-- Provider availability is shared across gateway replicas. Probe tokens fence
-- half-open requests so only one instance tests an endpoint after cooldown.
CREATE TABLE provider_circuit_breakers (
    provider_name         text PRIMARY KEY CHECK (provider_name <> ''),
    consecutive_failures  integer NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    opened_at             timestamptz,
    probe_token           uuid,
    probe_until           timestamptz,
    updated_at            timestamptz NOT NULL
);
