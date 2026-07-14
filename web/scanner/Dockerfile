# Build context = repo root (see compose.yaml): the pnpm workspace lockfile
# lives there, and corepack pins pnpm from the root packageManager field.
FROM node:26.5.0-alpine3.23@sha256:0473b6671ff22c8eeb570c0e1e51408595d3171e73f8002c269b763f0a943149 AS build
ENV CI=true
WORKDIR /src
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
RUN corepack enable pnpm && corepack install
COPY web/scanner/ web/scanner/
RUN pnpm install --frozen-lockfile && pnpm --filter scanner build

FROM nginx:1.31.2-alpine3.23@sha256:54f2a904c251d5a34adf545a72d32515a15e08418dae0266e23be2e18c66fefa
RUN sed -i -e 's!^pid.*!pid /tmp/nginx.pid;!' -e 's!/var/cache/nginx!/tmp/nginx!g' /etc/nginx/nginx.conf \
    && mkdir -p /tmp/nginx \
    && chown nginx:nginx /tmp/nginx
COPY web/scanner/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/web/scanner/dist /usr/share/nginx/html/scanner
USER nginx
