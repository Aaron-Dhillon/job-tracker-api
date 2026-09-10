-- gen_random_uuid() is core in PostgreSQL 13+, so no pgcrypto extension is
-- needed (and creating extensions is not permitted on Supabase's pooler).

create table users (
    id            uuid primary key default gen_random_uuid(),
    email         text not null unique,
    password_hash text not null,
    role          text not null check (role in ('user', 'admin')),
    created_at    timestamptz not null default now()
);

create table companies (
    id         uuid primary key default gen_random_uuid(),
    name       text not null unique,
    created_at timestamptz not null default now()
);

create table applications (
    id         uuid primary key default gen_random_uuid(),
    user_id    uuid not null references users(id) on delete cascade,
    company_id uuid not null references companies(id),
    role_title text not null,
    location   text,
    notes      text,
    status     text not null default 'applied'
               check (status in ('applied', 'screening', 'interviewing', 'offer', 'rejected', 'withdrawn')),
    applied_on date not null default current_date,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    -- The two-argument to_tsvector is required here: the single-argument form
    -- is only STABLE, and a generated column demands an IMMUTABLE expression.
    search_vec tsvector generated always as (
                   to_tsvector('english',
                       coalesce(role_title, '') || ' ' ||
                       coalesce(location, '')   || ' ' ||
                       coalesce(notes, '')
                   )
               ) stored
);

create index applications_search_idx on applications using gin (search_vec);
create index applications_user_idx on applications (user_id);
create index applications_company_idx on applications (company_id);

create table status_transitions (
    id             bigserial primary key,
    application_id uuid not null references applications(id) on delete cascade,
    from_status    text not null,
    to_status      text not null,
    -- on delete cascade: without it, a user who authored transitions can never
    -- be deleted, since users -> applications already cascades.
    changed_by     uuid not null references users(id) on delete cascade,
    changed_at     timestamptz not null default now()
);

create index status_transitions_app_idx on status_transitions (application_id, changed_at);
