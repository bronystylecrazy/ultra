-- +goose Up
create table ducks (
    id uuid primary key default gen_random_uuid(),
    subject text not null,
    owner_id uuid not null,
    name text not null,
    weight integer not null,
    created_at timestamptz not null,
    unique (subject, name)
);
create index ducks_page_idx on ducks (subject, created_at desc, id desc);

create table quacks (
    id uuid primary key default gen_random_uuid(),
    duck_id uuid not null references ducks (id),
    loudness integer not null,
    created_at timestamptz not null
);

-- +goose Down
drop table quacks;
drop table ducks;
