CREATE SCHEMA sales;

CREATE TABLE main.orders (
    id INTEGER PRIMARY KEY,
    customer_id INTEGER,
    total DECIMAL(12, 2) NOT NULL
);
CREATE TABLE sales.orders (
    order_code VARCHAR PRIMARY KEY,
    amount DOUBLE,
    region VARCHAR
);
CREATE TABLE sales."MixedCase" (
    id INTEGER PRIMARY KEY,
    note VARCHAR
);

CREATE TABLE main.customers (
    id INTEGER PRIMARY KEY,
    name VARCHAR NOT NULL
);
CREATE INDEX idx_customers_name ON main.customers (name);
CREATE INDEX idx_lower ON main.customers (lower(name));

CREATE TABLE main.composite_pk (
    tenant_id INTEGER,
    item_id INTEGER,
    value VARCHAR,
    PRIMARY KEY (tenant_id, item_id)
);
CREATE TABLE main.unique_nonnull (
    code VARCHAR NOT NULL UNIQUE,
    description VARCHAR
);
CREATE TABLE main.nullable_unique (
    code VARCHAR UNIQUE,
    description VARCHAR
);
CREATE TABLE main.no_key (
    label VARCHAR
);

CREATE TABLE main.parent_rows (
	    tenant_id INTEGER,
	    id INTEGER,
	    label VARCHAR,
	    PRIMARY KEY (tenant_id, id)
);
CREATE TABLE main.child_rows (
	    id INTEGER PRIMARY KEY,
	    parent_tenant_id INTEGER,
	    parent_id INTEGER,
	    CONSTRAINT fk_child_parent FOREIGN KEY (parent_tenant_id, parent_id)
	        REFERENCES main.parent_rows(tenant_id, id)
);

CREATE SEQUENCE main.seq_id;
CREATE TABLE main.seq_t (
    id BIGINT PRIMARY KEY DEFAULT nextval('seq_id'),
    value VARCHAR
);

CREATE VIEW main.v_orders AS SELECT id, customer_id, total FROM main.orders;

CREATE MACRO main.add1(x) AS x + 1;
CREATE MACRO main.top_orders(n) AS TABLE SELECT * FROM main.orders LIMIT n;
