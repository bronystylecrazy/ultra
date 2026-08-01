-- +goose Up
create table gizmos (
    id uuid primary key default gen_random_uuid(),
    subject text not null,
    label text,
    tally integer,
    ratio double precision,
    active boolean,
    tag uuid,
    seen_at timestamptz,
    payload jsonb,
    created_at timestamptz not null,
    unique (subject, label)
);
create index gizmos_page_idx on gizmos (subject, created_at desc, id desc);

create table gadgets (
    id uuid primary key default gen_random_uuid(),
    gizmo_id uuid not null references gizmos (id),
    note text,
    created_at timestamptz not null
);

-- +goose Down
drop table gadgets;
drop table gizmos;
