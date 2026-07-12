# Docker Guide

This guide covers building and running the application with Docker.

## Building the Image

```bash
make docker-build
# or
docker build -t ticketing_system:latest .
```

## Running the Container

```bash
make docker-run
# or
docker run -d --name ticketing_system ticketing_system:latest
```

## How Files Are Handled

The Docker image does **not** copy source files directly. Instead:

1. **The project is built** and installed into a virtual environment (`.venv`)
2. **Only the `.venv`** is copied to the final image
3. **The `conf/` directory** is explicitly copied to `/app/conf/`

This means your Python code is compiled and included in the venv, but other files (static assets, data files, etc.) are **not** included by default.

### Including Static Files

If you need static files available at runtime, place them in the `conf/` directory and reference them using the configuration path:

```python
from ticketing_system.app_config import AppConfig

config = AppConfig.instance()

# Correct - uses the configured path
file_path = config.config_path / "my-file.txt"

# Incorrect - hardcoded path won't work in Docker
# file_path = Path("./conf/my-file.txt")
```

This ensures the path works correctly both locally (`./conf/`) and in Docker (`/app/conf/`).

### Including Files Outside `conf/`

If you need to include files from other directories, update the Dockerfile to copy them:

```dockerfile
# Add after the COPY ${PROJECT_CONF_DIR} line
COPY ./my-data-dir /app/my-data-dir/
```

## Environment Variables

Pass environment variables to configure the application:

```bash
docker run -d \
  --name ticketing_system \
  -e ENV=prod \
  -e APP_NAME="Production App" \
  ticketing_system:latest
```

The image sets these defaults:
- `CONFIG_SOURCE=/app/conf`
- `ENVIRONMENT=prod`

## Common Operations

### View Logs

```bash
docker logs ticketing_system
docker logs -f ticketing_system  # Follow logs
```

### Stop Container

```bash
docker stop ticketing_system
docker rm ticketing_system
```

### Rebuild and Run

```bash
docker stop ticketing_system && docker rm ticketing_system
make docker-build
make docker-run
```

## Production Deployment

For production deployments, consider:

1. **Multi-stage builds** - The Dockerfile uses multi-stage builds to minimize image size
2. **Environment configuration** - Use environment variables or mounted config files
3. **Health checks** - Configure container health checks
4. **Resource limits** - Set memory and CPU limits

```bash
docker run -d \
  --name ticketing_system \
  --memory=512m \
  --cpus=1.0 \
  -e ENV=prod \
  ticketing_system:latest
```

## Mounting Configuration

To override configuration at runtime without rebuilding:

```bash
docker run -d \
  --name ticketing_system \
  -v /path/to/local/conf:/app/conf:ro \
  -e ENV=prod \
  ticketing_system:latest
```
