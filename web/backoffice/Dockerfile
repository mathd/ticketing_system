# Build context = repo root (see compose.yaml): the pnpm workspace lockfile
# lives there. Node 25+ dropped the bundled corepack, so pnpm is installed
# directly, pinned to the root packageManager version (pnpm@12.18.0).
# @astrojs/node standalone bundles runtime deps into dist/server, so the
# runtime stage ships dist only.
FROM node:26.13.0-alpine3.24@sha256:8996bb5821d62a8fd9bb27c1cb07462f3fdce6e47d640c6a881ead0b064ce216 AS build
ENV CI=true ASTRO_TELEMETRY_DISABLED=1
WORKDIR /src
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
RUN npm install -g pnpm@12.18.0
# openapi-typescript output is committed; the spec is not needed in-image.
COPY web/backoffice/ web/backoffice/
RUN pnpm install --frozen-lockfile --filter backoffice && pnpm --filter backoffice build

FROM node:26.13.0-alpine3.24@sha256:8996bb5821d62a8fd9bb27c1cb07462f3fdce6e47d640c6a881ead0b064ce216
ENV HOST=0.0.0.0 PORT=8080 NODE_ENV=production
WORKDIR /app
COPY --from=build /src/web/backoffice/dist ./dist
COPY --from=build /src/web/backoffice/start.mjs /src/web/backoffice/credentials.mjs ./
EXPOSE 8080
USER node
# start.mjs, not entry.mjs: it refuses to start when the two staff credentials
# are missing or identical, BEFORE the server listens (TKT-194). Astro defers
# middleware to the first request, so a check there cannot fail the process.
CMD ["node", "start.mjs"]
