# Development Guide

This guide covers development workflows, tooling, and project conventions.

## Available Commands

| Command | Description |
|---------|-------------|
| `make sync` | Install/sync all dependencies |
| `make run` | Run the application locally |
| `make test` | Run the test suite |
| `make lint` | Check linting with ruff |
| `make format` | Check formatting with ruff |
| `make lx` | Fix lint issues |
| `make fx` | Fix formatting issues |
| `make x` | Fix both lint and formatting |
| `make types` | Run type checking with pyright |
| `make pc` | Run pre-commit on all files |
| `make pci` | Install pre-commit hooks |
| `make docker-build` | Build Docker image |
| `make docker-run` | Run Docker container |

## Dependency Locking

- Install dependencies with `make sync` or `uv sync --locked --all-groups --all-extras`.
- `exclude-newer = "3 days"` tells `uv` to avoid resolving or downloading package releases published in the last 3 days, which reduces exposure to freshly published supply-chain compromises.
- Run `uv lock` only when you intentionally want to update dependencies.
- Commit `uv.lock` alongside dependency changes.

## Code Quality Tools

This project uses the following tools to maintain code quality:

### Ruff

[Ruff](https://docs.astral.sh/ruff/) handles both linting and formatting.

```bash
# Check for lint issues
make lint
# or: uv run ruff check

# Check formatting
make format
# or: uv run ruff format --check

# Fix lint issues
make lx
# or: uv run ruff check --fix

# Fix formatting
make fx
# or: uv run ruff format

# Fix both
make x
```

### Pyright

[Pyright](https://github.com/microsoft/pyright) provides static type checking in strict mode.

```bash
make types
# or: uv run pyright
```

Type checking is configured for `src/**/*.py` files. Tests are excluded.

### Pre-commit

[Pre-commit](https://pre-commit.com/) runs automated checks before each commit.

**Install hooks:**
```bash
make pci
# or:
uv run pre-commit install
uv run pre-commit install -t commit-msg
```

**Run manually:**
```bash
make pc
# or: uv run pre-commit run --all-files
```

**Configured hooks:**
- trailing-whitespace
- end-of-file-fixer
- check-yaml
- check-added-large-files
- uv-lock
- ruff (lint + format)
- pyright
- commitizen (commit message validation)

## Project Structure

This project follows the [src layout](https://packaging.python.org/en/latest/discussions/src-layout-vs-flat-layout/).

```
ticketing_system/
├── conf/                       # Configuration files
│   ├── base/                   # Base settings (shared across environments)
│   ├── local/                  # Local development settings (gitignored)
│   ├── prod/                   # Production settings
│   └── test/                   # Test settings
├── docs/                       # Documentation
│   ├── adr/                    # Architecture Decision Records
│   └── architecture.md         # System architecture
├── src/
│   └── ticketing_system/                # Main application package
│       ├── libs/               # Application-specific libraries
│       │   ├── config/         # Configuration utilities
│       │   └── utils/          # Shared utilities
│       ├── __init__.py
│       ├── __main__.py         # Entry point
│       └── app_config.py       # Application configuration
├── tests/                      # Test suite
│   └── ticketing_system/                # Tests mirroring src structure
├── AGENTS.md                   # AI agent guidance
├── Dockerfile
├── Makefile
├── pyproject.toml
└── README.md
```

## Import Guidelines

Imports **must not** use the `src.` prefix. Use direct package imports:

```python
# Correct
from ticketing_system import foo
from ticketing_system.libs.config import BaseConfig
from ticketing_system.libs.utils import bar

# Incorrect - will fail in production
from src.ticketing_system import foo
```

A ruff rule enforces this restriction.

## Code Style

For comprehensive code style guidelines including:
- Naming conventions
- Type annotations
- Error handling patterns
- Docstring format
- Logging conventions

See [AGENTS.md](../AGENTS.md) in the project root.
