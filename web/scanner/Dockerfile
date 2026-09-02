# Build context = repo root (see compose.yaml): the pnpm workspace lockfile
# lives there. Node 25+ dropped the bundled corepack, so pnpm is installed
# directly, pinned to the root packageManager version (pnpm@11.12.0).
FROM node:26.8.1-alpine3.23@sha256:871eb674ad6e692c91330a8959f1ce2f80ba3f445cdc54e306869d2ea265e42d AS build
ENV CI=true
WORKDIR /src
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
RUN npm install -g pnpm@11.12.0
COPY web/scanner/ web/scanner/
RUN pnpm install --frozen-lockfile && pnpm --filter scanner build

FROM nginx:1.31.2-alpine3.23@sha256:54f2a904c251d5a34adf545a72d32515a15e08418dae0266e23be2e18c66fefa
RUN sed -i -e 's!^pid.*!pid /tmp/nginx.pid;!' -e 's!/var/cache/nginx!/tmp/nginx!g' /etc/nginx/nginx.conf \
    && mkdir -p /tmp/nginx \
    && chown nginx:nginx /tmp/nginx
COPY web/scanner/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/web/scanner/dist /usr/share/nginx/html/scanner
USER nginx
