-- Schema version 2: the rollout under way of a service (D24). The new
-- instance runs beside the old one; both keep their own generation and
-- address until the old one has drained. At most one per service.
CREATE TABLE deployments (
    service        text PRIMARY KEY REFERENCES services (name) ON DELETE CASCADE,
    phase          text NOT NULL,
    spec           bytea NOT NULL,
    version        bigint NOT NULL,
    generation     bigint NOT NULL,
    node           text NOT NULL,
    addr           text NOT NULL DEFAULT '',
    old_spec       bytea NOT NULL,
    old_version    bigint NOT NULL,
    old_generation bigint NOT NULL,
    old_node       text NOT NULL,
    old_addr       text NOT NULL,
    since          timestamptz NOT NULL
);
