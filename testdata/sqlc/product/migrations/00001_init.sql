-- +goose Up
create table notes (
    id uuid primary key default gen_random_uuid(),
    subject text not null,
    title text not null,
    body text not null,
    weight integer not null,
    pinned boolean not null default false,
    created_at timestamptz not null,
    unique (subject, title)
);
create index notes_page_idx on notes (subject, created_at desc, id desc);

create table comments (
    id uuid primary key default gen_random_uuid(),
    note_id uuid not null references notes (id),
    body text not null,
    created_at timestamptz not null
);
create unique index comments_note_body_uq on comments (note_id, body);

create table shapes (
    id uuid primary key default gen_random_uuid(),
    area box not null,
    created_at timestamptz not null
);

-- +goose Down
drop table shapes;
drop table comments;
drop table notes;
