# Architecture Overview

This document contains the architecture diagrams for the project. Each diagram should be kept up to date as the system evolves. See [Technical Delivery Standards](./technical-delivery-standards.md#5-architecture-diagrams) for requirements.

## System Architecture

High-level view of the solution. This diagram must cover:
- **Inputs & outputs** — What enters and leaves the system
- **Application layer** — APIs, services, workers (if applicable)
- **AI / ML components** — Models, agents, pipelines
- **Data** — Databases, storage, caches
- **External integrations** — Third-party systems and dependencies
- **Ops & observability** — Monitoring, logging, alerting
- **Network & security** — Access patterns, authentication

> Replace the examples below with your project's actual architecture. Keep one that matches your project type and remove the other.

### Example: Agentic Chatbot

```mermaid
flowchart TB
    subgraph Client
        UI[Web App / Chat UI]
    end

    subgraph API Layer
        GW[API Gateway]
        Auth[Auth / IAM]
        BE[Backend - FastAPI]
    end

    subgraph Agent Orchestration
        AO[Agent Router]
        LLM[LLM - Claude]
        LLM_FB[LLM Fallback - GPT-4o]
        GR[Guardrails & Content Filter]
    end

    subgraph RAG
        EMB[Embedding Model]
        VDB[(Vector DB - Pinecone)]
        RR[Reranker]
    end

    subgraph Tools
        T1[Search Docs]
        T2[Create Ticket - Jira]
        T3[Query DB - Read Only]
    end

    subgraph Data
        DB[(PostgreSQL - Conversations)]
        S3[Object Storage - Documents]
        Cache[(Redis - Cache)]
    end

    subgraph Observability
        MON[Monitoring - Datadog]
        LOG[Logging - CloudWatch]
        TRACE[Tracing - LangSmith]
    end

    UI -->|HTTPS| GW
    GW --> Auth
    Auth --> BE
    BE --> AO
    AO --> LLM
    AO -.->|fallback| LLM_FB
    AO --> GR
    AO --> T1 & T2 & T3

    AO -->|query| EMB
    EMB --> VDB
    VDB --> RR
    RR -->|context| AO

    BE --> DB
    BE --> Cache
    S3 -->|ingestion| EMB

    BE --> MON & LOG
    AO --> TRACE
```

### Example: ML Classic (Demand Forecasting)

```mermaid
flowchart TB
    subgraph Data Sources
        ERP[(ERP - Sales History)]
        IOT[IoT Sensors / Inventory]
        EXT[External API - Weather, Events]
    end

    subgraph Ingestion & Transformation
        ORCH[Orchestrator - Airflow]
        ETL[ETL Pipeline]
        DQ[Data Quality Checks]
        FS[(Feature Store)]
    end

    subgraph Training
        TRAIN[Model Training - LightGBM]
        EVAL[Evaluation & Metrics]
        REG[Model Registry - MLflow]
        EXP[Experiment Tracking - MLflow]
    end

    subgraph Serving
        API[Forecast API - FastAPI]
        BATCH[Batch Forecasting - Daily]
        AB[A/B Testing]
    end

    subgraph Data
        DW[(Data Warehouse)]
        S3[Object Storage - Artifacts]
    end

    subgraph Observability
        MON[Monitoring - Drift Detection]
        LOG[Logging - Datadog]
        ALERT[Alerting - PagerDuty]
    end

    ERP & IOT & EXT --> ORCH
    ORCH --> ETL
    ETL --> DQ
    DQ --> FS
    DQ --> DW

    FS --> TRAIN
    TRAIN --> EVAL
    EVAL --> REG
    TRAIN --> EXP
    REG --> S3

    REG --> API
    REG --> BATCH
    API --> AB

    API --> MON & LOG
    MON --> ALERT
    BATCH --> DW
```

## Cloud Infrastructure

Technical and operational view of the infrastructure. This diagram must cover:
- **Cloud resources** — Compute, storage, networking configurations
- **Network & security** — VPC, subnets, firewall rules, IAM, secrets management
- **Environment isolation** — Dev, staging, prod and how they are separated

### Example: Email Classification on GCP (Private Network)

```mermaid
flowchart TB
    subgraph External
        SRC[Email Source - Exchange / Gmail API]
    end

    subgraph GCP - VPC Private Network
        subgraph Ingestion
            GCS[(Cloud Storage - Raw Emails)]
            PS_INGEST[Pub/Sub - New Email]
            CR_IDX[Cloud Run - Indexing Service]
        end

        subgraph Vector Store
            VDB[(AlloyDB / pgvector)]
        end

        subgraph Classification
            PS_CLASSIFY[Pub/Sub - Ready to Classify]
            CR_CLS[Cloud Run - Classification Service]
            LLM[LLM API - Claude / Vertex AI]
        end

        subgraph Event Routing
            PS_RESULT[Pub/Sub - Classification Result]
            FAN[Downstream Services / Notifications]
        end

        subgraph Data
            BQ[(BigQuery - Results & Analytics)]
        end

        subgraph Security
            IAM[IAM & Service Accounts]
            SM[Secret Manager]
            NAT[Cloud NAT - Outbound Only]
        end

        subgraph Observability
            LOG[Cloud Logging]
            MON[Cloud Monitoring]
            ALERT[Alerting Policy]
        end
    end

    SRC -->|fetch| GCS
    GCS -->|notification| PS_INGEST
    PS_INGEST -->|push| CR_IDX
    CR_IDX -->|embed & store| VDB
    CR_IDX -->|publish| PS_CLASSIFY

    PS_CLASSIFY -->|push| CR_CLS
    CR_CLS -->|retrieve context| VDB
    SM -->|API keys| CR_CLS
    CR_CLS -->|classify| LLM
    CR_CLS -->|results| BQ
    CR_CLS -->|publish| PS_RESULT

    PS_RESULT --> FAN

    NAT -->|outbound to LLM API| LLM
    IAM --> CR_IDX & CR_CLS & GCS & BQ

    CR_IDX & CR_CLS --> LOG
    LOG --> MON
    MON --> ALERT
```

## Data Pipeline

End-to-end view of data flows. This diagram must cover:
- **Sources** — Where data originates and how it is ingested (batch, streaming, API)
- **Transformations** — Processing steps and intermediate storage
- **Serving** — How data is consumed by the solution

### Example: Multi-Source to Snowflake to Azure ML

```mermaid
flowchart LR
    subgraph Source Systems
        ERP[(ERP - SAP)]
        CRM[(CRM - Salesforce)]
        IOT[IoT / Streaming]
        API[Third-Party APIs]
        FLAT[Flat Files - SFTP / S3]
    end

    subgraph Google Cloud
        BQ[(BigQuery)]
    end

    subgraph Ingestion & Orchestration
        ADF[Azure Data Factory]
        DBX[Databricks / Spark Jobs]
    end

    subgraph Snowflake
        RAW[Raw Layer]
        STG[Staging Layer - dbt]
        CUR[Curated Layer - dbt]
        FEAT[Feature Store / ML Views]
    end

    subgraph Azure ML
        DS[ML Dataset]
        TRAIN[Training Pipeline]
        REG[Model Registry]
        EP[Managed Endpoint]
    end

    ERP & CRM & API & FLAT -->|batch| ADF
    IOT -->|streaming| DBX
    ADF --> RAW
    DBX --> RAW
    BQ -->|Snowflake External Tables / ADF| RAW

    RAW --> STG
    STG --> CUR
    CUR --> FEAT

    FEAT -->|export| DS
    DS --> TRAIN
    TRAIN --> REG
    REG --> EP
```

_Describe deployment topology: where the service runs, what it depends on (databases, queues, external APIs)._

## Decisions

See [`adr/`](./adr/) for architecture decision records.
