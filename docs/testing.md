# Testing Guide

This guide covers running tests and writing new tests for the project.

## Running Tests

### Basic Commands

```bash
# Run all tests
make test
# or: uv run pytest

# Run with verbose output
uv run pytest -v

# Run with coverage report
uv run pytest --cov=src/ticketing_system
```

### Running Specific Tests

```bash
# Run a single test file
uv run pytest tests/ticketing_system/test_app_config.py

# Run a specific test function
uv run pytest tests/ticketing_system/test_app_config.py::test_app_config

# Run tests matching a pattern
uv run pytest -k "test_config"

# Run tests in a specific directory
uv run pytest tests/ticketing_system/
```

### Test Options

```bash
# Stop on first failure
uv run pytest -x

# Run last failed tests
uv run pytest --lf

# Run tests in parallel (if pytest-xdist installed)
uv run pytest -n auto

# Show local variables in tracebacks
uv run pytest -l

# Generate HTML coverage report
uv run pytest --cov=src/ticketing_system --cov-report=html
```

## Writing Tests

### Test Structure

Tests are located in `tests/` and mirror the `src/` structure:

```
tests/
├── conftest.py              # Shared fixtures
└── ticketing_system/
    └── test_app_config.py   # Tests for app_config module
```

### Fixtures

Common fixtures are defined in `tests/conftest.py`:

```python
import pytest
from ticketing_system.app_config import AppConfig

@pytest.fixture
def app_config(monkeypatch: pytest.MonkeyPatch) -> AppConfig:
    monkeypatch.setenv("ENV", "test")
    return AppConfig()
```

### Example Test

```python
from ticketing_system.app_config import AppConfig

def test_app_config(app_config: AppConfig):
    assert app_config is not None
    assert "test" in app_config.settings.app_name
```

### Environment Variables

Use `monkeypatch` for environment variable manipulation:

```python
def test_with_custom_env(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("ENV", "prod")
    monkeypatch.setenv("APP_NAME", "Custom App")
    # ... test code
```

## Test Configuration

Test settings are loaded from `conf/test/` directory. The test environment is activated by setting `ENV=test`.

### pytest Configuration

pytest is configured in `pyproject.toml`. Key settings:
- `pytest-asyncio` for async test support
- `pytest-cov` for coverage reporting

## Best Practices

1. **Naming**: Prefix test functions with `test_`
2. **Fixtures**: Use fixtures for common setup
3. **Isolation**: Each test should be independent
4. **Environment**: Use `monkeypatch` for env vars, don't modify global state
5. **Assertions**: Use clear, specific assertions
