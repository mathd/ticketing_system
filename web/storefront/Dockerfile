# Build context = repo root (see compose.yaml): the pnpm workspace lockfile
# lives there. Node 25+ dropped the bundled corepack, so pnpm is installed
# directly, pinned to the root packageManager version (pnpm@12.21.0).
# @astrojs/node standalone bundles runtime deps into dist/server, so the
# runtime stage ships dist only.
FROM node:26.13.1-alpine3.24@sha256:6bd9ab43c0ec0190ca7b2d193906cb8b7edf6e7f4afa43ddeccd6452710bffb5 AS build
ENV CI=true ASTRO_TELEMETRY_DISABLED=1
WORKDIR /src
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
RUN npm install -g pnpm@12.21.0
# openapi-typescript output is committed; the spec is not needed in-image.
COPY web/scanner/package.json web/scanner/
COPY web/storefront/ web/storefront/
RUN pnpm install --frozen-lockfile --filter storefront && pnpm --filter storefront build

FROM node:26.13.1-alpine3.24@sha256:6bd9ab43c0ec0190ca7b2d193906cb8b7edf6e7f4afa43ddeccd6452710bffb5
ENV HOST=0.0.0.0 PORT=8080 NODE_ENV=production
WORKDIR /app
COPY --from=build /src/web/storefront/dist ./dist
EXPOSE 8080
USER node
CMD ["node", "dist/server/entry.mjs"]
