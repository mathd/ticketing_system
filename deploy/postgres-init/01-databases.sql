-- One database + one role per service (ADR-007). CONNECT is revoked from
-- PUBLIC on every service database so a service's credentials physically
-- cannot reach another service's data — asserted by the smoke suite.
\set ON_ERROR_STOP on

CREATE ROLE catalog   LOGIN PASSWORD 'catalog';
CREATE ROLE inventory LOGIN PASSWORD 'inventory';
CREATE ROLE commerce  LOGIN PASSWORD 'commerce';
CREATE ROLE payments  LOGIN PASSWORD 'payments';
CREATE ROLE access    LOGIN PASSWORD 'access';

CREATE DATABASE catalog   OWNER catalog;
CREATE DATABASE inventory OWNER inventory;
CREATE DATABASE commerce  OWNER commerce;
CREATE DATABASE payments  OWNER payments;
CREATE DATABASE access    OWNER access;

REVOKE CONNECT ON DATABASE catalog   FROM PUBLIC;
REVOKE CONNECT ON DATABASE inventory FROM PUBLIC;
REVOKE CONNECT ON DATABASE commerce  FROM PUBLIC;
REVOKE CONNECT ON DATABASE payments  FROM PUBLIC;
REVOKE CONNECT ON DATABASE access    FROM PUBLIC;

GRANT CONNECT ON DATABASE catalog   TO catalog;
GRANT CONNECT ON DATABASE inventory TO inventory;
GRANT CONNECT ON DATABASE commerce  TO commerce;
GRANT CONNECT ON DATABASE payments  TO payments;
GRANT CONNECT ON DATABASE access    TO access;
