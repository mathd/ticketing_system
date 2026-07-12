<!-- TODO: This file is being trimmed to be a high-level design doc, not a runbook. Still open:
- Verify Tech Stack / RAG / Evaluation tables read as *decisions*, not as source-of-truth lists (source should be code/config).
- "Environments & Deployment" partially overlaps with continuous-delivery.md — consider trimming the release-process subsection.
- Consider splitting GenAI-specific sections (Prompts & Guardrails, RAG / Agent Architecture) into their own file when projects need them.
-->
# Solution Design

> **A design doc, not a runbook.** This file captures the high-level choices, requirements, and contracts behind the solution. Operational details — endpoint behavior, prompt content, schema specifics, environment configs — belong next to the code where they apply. If you find yourself maintaining low-level details here, update the source instead.

Description of the solution being designed.

## Problem Definition

Short Description: XYZ

| Aspect | Description |
|---|---|
| **Problem Type** | _Agentic / Classification / Regression / Clustering / Recommendation / Other_ |
| **Objective** | _e.g. Predict churn within 30 days / Generate contextual responses for customer support_ |
| **Input** | _e.g. Customer features (demographics, behavior) / User question + conversation history_ |
| **Output** | _e.g. Churn probability (0-1) + class (High/Medium/Low) / Natural language response with citations_ |
| **Approach** | _e.g. Supervised ML (XGBoost) / GenAI with RAG (Claude + Pinecone)_ |
| **XYZ** | _e.g. XYZ_ |

---

## Architecture & Diagrams

System, infrastructure, and data pipeline diagrams live in [architecture.md](./architecture.md). Diagram requirements (what each must cover) are defined in [technical-delivery-standards.md §5](./technical-delivery-standards.md#5-architecture-diagrams). For IaC definitions, see the `infra/` directory if applicable.

---

## Tech Stack

Technologies chosen and their justification. Major changes should be reflected in an [ADR](./adr/).

Concrete versions and dependencies live in the repo:
- [`pyproject.toml`](../pyproject.toml) — Python dependencies and versions
- [`Dockerfile`](../Dockerfile) — Runtime environment
- [`.pre-commit-config.yaml`](../.pre-commit-config.yaml) — Development tooling

### Core

| Component | Technology | Justification |
|---|---|---|
| Cloud | _e.g. AWS, GCP, Azure_ | _e.g. Existing client infrastructure_ |
| Backend | _e.g. Python + FastAPI_ | _e.g. Performance, async support_ |
| Database | _e.g. PostgreSQL_ | _e.g. Robust, ACID compliant_ |
| Storage | _e.g. S3, GCS_ | _e.g. Documents, artifacts, logs_ |
| Orchestration | _e.g. Airflow, Prefect_ | _e.g. Pipeline scheduling_ |

### ML _(if applicable)_

| Component | Technology | Justification |
|---|---|---|
| Experiment Tracking | _e.g. MLflow_ | _e.g. Runs, metrics, artifacts_ |
| Model Registry | _e.g. MLflow Registry_ | _e.g. Versioning, staging/prod_ |
| Model Serving | _e.g. SageMaker, FastAPI_ | _e.g. Auto-scaling, A/B testing_ |
| Feature Store | _e.g. Feast, S3_ | _e.g. Reusability, versioning_ |
| Data Versioning | _e.g. Delta Lake_ | _e.g. Reproducibility_ |

### Agentic _(if applicable)_

| Component | Technology | Justification |
|---|---|---|
| LLM | _e.g. Claude 3.5 Sonnet_ | _e.g. Performance, safety_ |
| LLM Fallback | _e.g. GPT-4o_ | _e.g. Resilience_ |
| Agentic Framework | _e.g. ADK, LangChain_ | _e.g. Client ecosystem fit_ |
| Vector DB | _e.g. Pinecone, pgvector_ | _e.g. Managed, performant_ |
| Embedding | _e.g. text-embedding-3-large_ | _e.g. High quality, 3072 dim_ |
| Prompt Versioning | _e.g. Git, Langfuse_ | _e.g. Version tracking, A/B testing_ |

### Observability

| Component | Technology | Justification |
|---|---|---|
| Monitoring | _e.g. Datadog, CloudWatch_ | _e.g. Drift, performance, alerting_ |
| Logging | _e.g. Datadog, ELK_ | _e.g. Centralized logs, debugging_ |
| Alerting | _e.g. PagerDuty, Grafana_ | _e.g. Incident response_ |

---

## External Integrations

Every external integration is a dependency. Identifying them clearly reduces blockers and facilitates communication with the relevant client teams.

| System | Type | Protocol | Auth | Client Contact |
|---|---|---|---|---|
| _e.g. CRM Salesforce_ | API REST | HTTPS | OAuth 2.0 | _Name, role, email_ |
| _e.g. Data Warehouse_ | Direct DB | PostgreSQL | Service account | _Name, role, email_ |
| _e.g. Email Service_ | API REST | HTTPS | API Key | _Name, role, email_ |

---

## Data Requirements _(if applicable)_

Data schemas, validation rules, and quality checks are defined in code to stay in sync with the implementation.

- **Input/Output schemas** — See model definitions in `src/` (e.g. Pydantic models, dataclasses)
- **Data validation** — See pipeline validation logic or quality check configurations
- **Data refresh strategy** — See orchestration configs (e.g. Airflow DAGs, pipeline schedules)

> Keep schema documentation as close to the code as possible. Use docstrings and type annotations as the source of truth.

### Data Refresh Strategy

| Dataset | Refresh Frequency | Method | Owner |
|---|---|---|---|
| _e.g. Training data_ | _Monthly_ | _Full reload_ | _Data Eng team_ |
| _e.g. Feature store_ | _Daily_ | _Incremental_ | _ML Pipeline_ |
| _e.g. Inference data_ | _Real-time_ | _Streaming (Kafka)_ | _Backend API_ |

---

## Prompts & Guardrails _(GenAI only)_

Document the prompting strategy and safety guardrails for GenAI solutions.

- **System prompt template** — Where it lives, how it's versioned (e.g. Git, prompt registry)
- **Guardrails** — Content filtering, PII detection, output validation
- **Human-in-the-loop** — Approval flows for critical actions, escalation paths

> For prompt implementations, see the relevant code in the repository. For prompt versioning, refer to the tech stack section above.

---

## Evaluation Strategy

How model/system quality is measured, monitored, and maintained over time.

### ML _(if applicable)_

| Aspect | Description |
|---|---|
| **Offline Metrics** | _e.g. Accuracy, Precision, Recall, F1, AUC-ROC, RMSE_ |
| **Business Metrics** | _e.g. Revenue impact, conversion rate, cost savings_ |
| **Baseline** | _e.g. Rule-based heuristic, current production model_ |
| **Eval Dataset** | _e.g. Holdout set, stratified by segment, refreshed quarterly_ |
| **Drift Monitoring** | _e.g. Feature drift (KL divergence), prediction drift, concept drift_ |
| **Retraining Trigger** | _e.g. Drift threshold exceeded, scheduled monthly, performance drop_ |
| **A/B Testing** | _e.g. Shadow mode, canary deployment, champion/challenger_ |

### Agentic / GenAI _(if applicable)_

| Aspect | Description |
|---|---|
| **Quality Metrics** | _e.g. LLM-as-judge, human eval, factual accuracy, relevance_ |
| **Regression Testing** | _e.g. Golden dataset of Q&A pairs, run on prompt changes_ |
| **Retrieval Metrics (RAG)** | _e.g. Recall@k, MRR, context relevance_ |
| **Cost Tracking** | _e.g. Tokens per request, cost per conversation, cache hit rate_ |
| **Latency Budgets** | _e.g. P50 < 2s, P95 < 5s, streaming first token < 500ms_ |
| **Safety Testing** | _e.g. Adversarial prompts, jailbreak attempts, PII leakage checks_ |

---

## RAG / Agent Architecture _(GenAI only)_

### RAG Pipeline _(if applicable)_

| Component | Description |
|---|---|
| **Chunking Strategy** | _e.g. Recursive text splitter, 500 tokens, 50 overlap_ |
| **Embedding Model** | _e.g. text-embedding-3-large, 3072 dimensions_ |
| **Retrieval** | _e.g. Top-k=5, hybrid search (semantic + keyword)_ |
| **Reranking** | _e.g. Cohere reranker, cross-encoder_ |
| **Context Window** | _e.g. 8k tokens max, truncation strategy_ |

### Knowledge Base Lifecycle

| Aspect | Description |
|---|---|
| **Sources** | _internal docs (this repo)_ |
| **Ingestion** | _e.g. Nightly batch, webhook on update_ |
| **Versioning** | _e.g. Document hash, re-index on change_ |
| **Cleanup** | _e.g. Remove stale docs after 90 days_ |

### Agent Tools & Permissions _(if applicable)_

| Tool | Description | Permissions | Risk Level |
|---|---|---|---|
| _e.g. search_docs_ | _Search knowledge base_ | _Read-only_ | _Low_ |
| _e.g. create_ticket_ | _Create Jira ticket_ | _Write_ | _Medium_ |
| _e.g. execute_sql_ | _Run SQL queries_ | _Read-only, allow-listed tables_ | _High_ |

---

## API Contracts _(if applicable)_

> **API documentation lives with the code.** Detailed endpoint behavior, request/response examples, and usage patterns belong in docstrings and a README next to the API code (e.g., `src/<module>/api/README.md`). This section is the **high-level contract index** — not the place to maintain endpoint documentation. If you find yourself updating endpoint behavior here, update the code instead.

This project follows a **code-first** approach: Pydantic models and FastAPI route decorators are the source of truth. The OpenAPI spec is auto-generated — do not maintain a separate hand-written spec.

**Available endpoints:**

- **Interactive docs** — `/docs` (Swagger UI) or `/redoc`
- **Raw spec** — `/openapi.json` (committed to repo for PR diffs)

### Endpoint Summary

| Method | Path | Description | Auth |
|---|---|---|---|
| _e.g. POST_ | _/api/v1/classify_ | _Classify an email_ | _API Key_ |
| _e.g. GET_ | _/api/v1/results/:id_ | _Get classification result_ | _API Key_ |
| _e.g. GET_ | _/api/v1/health_ | _Health check_ | _Public_ |

### Keeping the contract accurate

- Use `Field(description=..., example=...)` in Pydantic models — this is your documentation
- Use `tags=` and `summary=` on routes to organize `/docs`
- Keep per-module API documentation in a co-located `README.md` (next to the routes), not in this doc
- Validate responses against Pydantic models in tests: `MyResponse.model_validate(response.json())`
- Consider [Schemathesis](https://github.com/schemathesis/schemathesis) for automated contract fuzz testing

---

## Environments & Deployment

### Environments

| Environment | Purpose | URL / Access | Deployed From |
|---|---|---|---|
| _e.g. dev_ | _Development and integration testing_ | _e.g. dev.project.client.com_ | _Feature branches (auto)_ |
| _e.g. staging_ | _Pre-production validation, client UAT_ | _e.g. staging.project.client.com_ | _`main` branch (auto)_ |
| _e.g. prod_ | _Production_ | _e.g. project.client.com_ | _Release tag (manual approval)_ |

### Deployment Strategy

- **How**: _e.g. GitHub Actions → Docker build → push to registry → deploy to Cloud Run / EKS / AKS_
- **IaC**: _e.g. Terraform modules in `infra/`, applied via CI pipeline_
- **Secrets**: _e.g. Managed via Secret Manager / Key Vault, injected at runtime_
- **Rollback**: _e.g. Revert to previous container revision / redeploy last tag_

### Release Process

1. Feature branches merge to `main` via PR (deploys to dev/staging automatically)
2. When ready for production, create a release tag (e.g. `v1.2.0`)
3. Production deploy requires manual approval in CI pipeline
4. Verify via health check and monitoring dashboards

> For configuration per environment, see [Configuration Guide](./configuration.md). For Docker builds, see [Docker Guide](./docker.md). For infrastructure code, see the `infra/` directory.
