-- Schema version 1 of Andén's durable state. Desired state is what the API
-- writes (spec, version, desired_awake, desired_node, deleted); observed
-- state is what the reconciler writes (state, node, addr) and what agents
-- report (instances).
CREATE TABLE services (
    name          text PRIMARY KEY,
    spec          bytea NOT NULL,
    version       bigint NOT NULL,
    desired_awake boolean NOT NULL,
    desired_node  text NOT NULL DEFAULT '',
    deleted       boolean NOT NULL DEFAULT false,
    revision      bigint NOT NULL DEFAULT 1,
    state         text NOT NULL,
    node          text NOT NULL DEFAULT '',
    addr          text NOT NULL DEFAULT '',
    edge_port     integer NOT NULL UNIQUE
);

CREATE TABLE nodes (
    name       text PRIMARY KEY,
    addr       text NOT NULL,
    epoch      bigint NOT NULL,
    cpu_millis integer NOT NULL,
    memory_mib integer NOT NULL
);

CREATE TABLE instances (
    id         text NOT NULL,
    service    text NOT NULL,
    node       text NOT NULL,
    version    bigint NOT NULL,
    generation bigint NOT NULL,
    state      text NOT NULL,
    addr       text NOT NULL,
    seccomp    boolean NOT NULL,
    route      text NOT NULL,
    PRIMARY KEY (node, id)
);

CREATE INDEX instances_service ON instances (service);
