# Build context = repo root (see compose.yaml): the pnpm workspace lockfile
# lives there, and corepack pins pnpm from the root packageManager field.
FROM node:24-alpine AS build
ENV CI=true
WORKDIR /src
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
RUN corepack enable pnpm && corepack install
COPY web/scanner/ web/scanner/
RUN pnpm install --frozen-lockfile && pnpm --filter scanner build

FROM nginx:alpine
COPY web/scanner/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/web/scanner/dist /usr/share/nginx/html/scanner
