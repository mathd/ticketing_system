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
COPY web/scanner/package.json web/scanner/
COPY web/storefront/ web/storefront/
RUN pnpm install --frozen-lockfile --filter storefront && pnpm --filter storefront build

FROM node:26.13.0-alpine3.24@sha256:8996bb5821d62a8fd9bb27c1cb07462f3fdce6e47d640c6a881ead0b064ce216
ENV HOST=0.0.0.0 PORT=8080 NODE_ENV=production
WORKDIR /app
COPY --from=build /src/web/storefront/dist ./dist
EXPOSE 8080
USER node
CMD ["node", "dist/server/entry.mjs"]
