# Configuration System

This project uses a hierarchical configuration system based on [BaseConfig](../src/ticketing_system/libs/config/README.md) that automatically loads configuration files from environment-specific directories and merges them with environment variables.

## Overview

- Configuration is loaded from `conf/` directory in layers
- Layers are merged in order: base → environment-specific
- Environment variables override file-based configuration
- Supports YAML, TOML, and JSON file formats

## Directory Structure

```
conf/
├── base/           # Base configuration (always loaded first)
│   └── settings.toml
├── local/          # Local development (loaded when ENV=local)
│   └── settings_shared.toml
├── prod/           # Production (loaded when ENV=prod)
│   └── settings.toml
└── test/           # Test environment (loaded when ENV=test)
    └── settings.toml
```

## Quick Start

### 1. Define configuration models

#### 1.1. Add new settings field

Go to [`src/ticketing_system/app_config.py`](../src/ticketing_system/app_config.py) and extend `AppSettings` class:

```python
...

class AppSettings(BaseSettings):
    ... # Existing fields

    my_setting: str = "default_value"
```

#### 1.2. Add new config field to AppConfig

```python
class PipelinesSettings(BaseSettings):
    bronze: ... = ...
    silver: ... = ...
    gold: ... = ...

class AppConfig(BaseConfig):
    ... # Existing fields
    pipelines: PipelinesSettings = ConfigField(pattern=["pipelines*"])
```

### 2. Update configuration files

- Add `my_setting` to the appropriate configuration files in `conf/`:

    ```toml
    # conf/base/settings.toml
    my_setting = "base_value"
    ```

    ```toml
    # conf/local/settings_shared.toml
    my_setting = "local_value"
    ```

- Add `pipelines` configuration:

    ```toml
    # conf/base/pipelines.toml
    [bronze]
    path = "/data/bronze"

    [silver]
    path = "/data/silver"

    [gold]
    path = "/data/gold"
    ```

### 3. Use the Configuration

```python
from ticketing_system.app_config import AppConfig

config = AppConfig.instance()
print(f"App: {config.settings.app_name}")
print(f"Environment: {config.config_env}")
```

## Environment Variables

The environment is controlled by the `ENV` variable (default: "local"):

```bash
export ENV=prod
```

Settings that inherit from Pydantic's `BaseSettings` can be overridden:

```bash
export APP_NAME="Override App Name"
```

## Configuration Loading Order

1. **Base layer** (`conf/base/`) - loaded first
2. **Environment layer** (`conf/{env}/`) - merged on top
3. **Environment variables** - highest priority for BaseSettings fields

## Advanced Features

### Lazy Loading

Defer loading until first access:

```python
settings: AppSettings = ConfigField(pattern=["settings*"], lazy=True)
```

### Caching

Cache configuration values with TTL:

```python
settings: AppSettings = ConfigField(pattern=["settings*"], cache_ttl=300)  # 5 minutes
```

### Required Fields

Ensure configuration files exist:

```python
settings: AppSettings = ConfigField(pattern=["settings*"], required=True)
```

### Inspecting Configuration

```python
config = AppConfig.instance()

# Get all values as dictionary
all_values = config.config_dump()

# Get values with source file information
field_info = config.config_describe()
```

### Async Loading

For non-blocking configuration loading:

```python
await config.config_aload("settings", "logging")
```

### Configuration Freezing

Prevent further loading after initialization:

```python
config.frozen = True
```

## Error Handling

```python
from ticketing_system.libs.config import (
    ConfigError,
    ConfigFieldError,
    ConfigFieldValidationError,
    ConfigFileLoadError,
    ConfigFrozenError,
)

try:
    config = AppConfig.instance()
except ConfigFieldError as e:
    print(f"Missing required config: {e}")
except ConfigFileLoadError as e:
    print(f"Failed to load file: {e}")
```

## Supported File Formats

- **YAML** (`.yaml`, `.yml`)
- **JSON** (`.json`)
- **TOML** (`.toml`)

## Further Reading

For the complete API reference and advanced usage (plugins, custom loaders, async loading), see the [Configuration Library README](../src/ticketing_system/libs/config/README.md).
