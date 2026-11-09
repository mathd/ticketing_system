# Prepare a local demo

Local Compose only. These steps assemble existing procedures; they are not a new live demo proof.
The August [assessment](../demo-readiness.md) remains historical.

1. Use the [toolchain](../development.md#toolchain), run `make deps`, then `make up`.
   Expect healthy services. If Docker is unavailable, start the host's Docker service before retrying.
   For a port collision, choose free `GATEWAY_PORT` and `POSTGRES_PORT` values before `make up`;
   the test harness chooses its own shifted ports and prints them at startup.
2. Provision a staff account using [the stdin-only command](staff-accounts.md). No seeded staff
   password exists. Sign in at `/admin/`; success redirects to the admin landing page.
3. Create a venue, event, performance, and ticket type through the current authoring UI or
   [Catalog contract](../../services/catalog/api/openapi.yaml). Publish the performance. Inventory
   provisioning is asynchronous; check availability before starting the purchase narrative.
   No demo seed target exists. A fresh storefront can be empty.
4. Browse through `/`, reserve, and use the fake PSP checkout outcomes. Complete a successful
   order and open its ticket bundle. Rehearse the failure paths required by the demo.
5. Use [scanner enrollment](scanner.md), then rehearse accepted and refused results in real Chrome.
   This is a rehearsal, not venue-device qualification. Follow the scanner prerequisites and limits.
6. Run `make check` as a separate command. For a form-verification claim, use `make browser`;
   API smoke and a rendered page do not establish browser submission behavior.

Grafana is at the `GRAFANA_PORT` exposed by the dev overlay. Use the provisioned
[checkout dashboard](../verification/checkout-dashboard/README.md). No elapsed-time promise applies
to a cold build. Stop without deleting data with the Compose command in
[development](../development.md#everyday-loop); `make down` deletes volumes.
