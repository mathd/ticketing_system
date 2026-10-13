# Build context = repo root (see compose.yaml): the pnpm workspace lockfile
# lives there. Node 25+ dropped the bundled corepack, so pnpm is installed
# directly, pinned to the root packageManager version (pnpm@11.12.0).
# @astrojs/node standalone bundles runtime deps into dist/server, so the
# runtime stage ships dist only.
FROM node:26.11.1-alpine3.23@sha256:f1394b3ffc021bc3f4aaf363e70a8d9667f7a9a63538bc824d7cc583129fffad AS build
ENV CI=true ASTRO_TELEMETRY_DISABLED=1
WORKDIR /src
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
RUN npm install -g pnpm@11.12.0
# openapi-typescript output is committed; the spec is not needed in-image.
COPY web/scanner/package.json web/scanner/
COPY web/storefront/ web/storefront/
RUN pnpm install --frozen-lockfile --filter storefront && pnpm --filter storefront build

FROM node:26.11.1-alpine3.23@sha256:f1394b3ffc021bc3f4aaf363e70a8d9667f7a9a63538bc824d7cc583129fffad
ENV HOST=0.0.0.0 PORT=8080 NODE_ENV=production
WORKDIR /app
COPY --from=build /src/web/storefront/dist ./dist
EXPOSE 8080
USER node
CMD ["node", "dist/server/entry.mjs"]
