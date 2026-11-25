# Product Requirements Document (PRD)

**Product Name:** Autonomous Business Operating System (ABOS) (Working Title)  
**Document Version:** 1.0 (MVP $\rightarrow$ V1 Production Spec)  
**Author:** Principal Product Engineer  
**Target Domain:** Multi-Channel Commerce, Wholesale/B2B, Retail & Service Operations  

---

## 1. Executive Summary & Product Vision

### 1.1 Problem Statement
Mid-market and scaling businesses run on fragmented software stacks: storefronts (Shopify, BigCommerce), operational ERPs (Odoo, SAP), accounting platforms (Zoho Books), documentation (Notion), and customer channels (Phone/Voice, WhatsApp). Because these systems do not share an autonomous brain, human teams waste 40–60% of their day copying data, reconciling inventory, chasing vendor quotes, answering routine status calls, and manually building operational plans. Meanwhile, existing AI agent frameworks require heavy engineering setup (YAML files, raw API keys, complex node graphs) and lack deterministic financial guardrails.

### 1.2 Product Vision
Build a **Zero-Config Autonomous Business Operating System** that sits above existing enterprise tools via the Model Context Protocol (MCP). It fuses:
* **Hermes-Style Agent Execution:** Model-agnostic runtime with persistent memory, reusable skill playbooks, scheduled heartbeats, and specialist delegation.
* **Paperclip-Style Company Governance:** Goal-to-ticket hierarchy, visual agent org charts, hard budget circuit breakers, human approval gates, and complete audit traces.
* **Real-Time Voice & Omnichannel Execution:** Sub-800ms inbound/outbound voice agents and a live operational execution board (replacing legacy ERP data-entry forms).

### 1.3 Core Product Principles
* **Orchestrate, Don't Replace:** Keep Odoo, SAP, Zoho Books, Shopify, BigCommerce, and Notion as underlying systems of record; unify control in one real-time command plane.
* **Deterministic Math, Probabilistic Reasoning:** LLMs plan, converse, and classify—deterministic code handles inventory math, tax calculations, idempotency, and financial policy enforcement.
* **Exception-First UX:** Humans supervise live operational pipelines and approve high-risk exceptions rather than filling out forms.

---

## 2. Target Personas & Jobs-to-be-Done (JTBD)

| Persona | Core Pain Point | Primary Interface | Key Job-to-be-Done |
| :--- | :--- | :--- | :--- |
| **Founder / Operations Director** | Blind spots across sales channels, ERP stock, and accounting; slow manual planning. | Live Execution Board + Business Planner | Set business goals, review morning briefings, approve high-value overrides/POs in seconds. |
| **Store / Warehouse Manager** | Stock mismatches between Shopify/BigCommerce and Odoo/SAP; delayed dispatches. | Execution Lanes + Approval Side Drawer | Resolve inventory conflicts and trigger supplier reorders with 1-click sign-off. |
| **Customer / B2B Dealer / Vendor** | Long hold times for order tracking, bulk quotes, or delivery scheduling. | Phone (SIP/WebRTC) & WhatsApp Voice/Text | Get instant answers, place repeat orders, and receive invoices without waiting on human staff. |

---

## 3. Functional Requirements

### 3.1 Module 1: Enterprise MCP Gateway & Universal Connectors
* **FR-1.1 Plug-and-Play MCP Server Registry:** Support connecting external and internal MCP servers over Streamable HTTP and STDIO (`mcp-shopify`, `mcp-bigcommerce`, `mcp-odoo`, `mcp-sap`, `mcp-zoho-books`, `mcp-notion`, `mcp-whatsapp`).
* **FR-1.2 Zero-Secret Credential Vault:** Store OAuth 2.1 tokens and API credentials centrally; inject scoped tokens dynamically at runtime so LLMs never see raw secrets. Support Enterprise-Managed Authorization (EMA) for zero-touch enterprise SSO provisioning.
* **FR-1.3 Semantic Tool Virtualization:** Group raw ERP/storefront endpoints into clean, role-scoped semantic tools (e.g., `commerce.get_order`, `erp.reserve_stock`, `books.draft_invoice`, `docs.update_sop`) to minimize context bloat and tool selection errors.
* **FR-1.4 Canonical Entity Mapping:** Maintain a unified `external_id_map` linking every SKU, Customer, Vendor, Order, and Invoice across all connected systems via an immutable internal UUID.
* **FR-1.5 Inbound/Outbound DLP & Prompt-Injection Shield:** Strip PII/payment data and isolate untrusted external strings (e.g., customer order notes, emails) before feeding tool outputs back into LLM reasoning loops.

### 3.2 Module 2: Deterministic Kernel & Governance Engine
* **FR-2.1 Idempotency Guard:** Enforce deterministic idempotency keys on all inbound webhooks and outbound MCP write operations to eliminate duplicate invoices or double stock deductions.
* **FR-2.2 Natural-Language Policy Compiler:** Allow users to write rules in plain English (e.g., *"Pause any refund above ₹1,000 or discount above 12% for manager approval"*) and compile them into deterministic pre-execution policy checks.
* **FR-2.3 Approval & Conflict Gates:** Automatically suspend high-risk writes (`PENDING_APPROVAL`) or cross-ERP discrepancies (`CONFLICT_HOLD`) and push actionable cards to the UI side drawer.
* **FR-2.4 Budget Circuit Breakers:** Enforce per-agent and per-plan token/API spend caps with 80% alerts and automatic hard stops at 100%.
* **FR-2.5 Decision X-Ray (Audit Log):** Record every agent execution step (trigger event, data read, policy evaluated, tool called, cost incurred) in an immutable trace.

### 3.3 Module 3: AI Business Planner & Multi-Agent Workforce
* **FR-3.1 Hierarchical Goal Decomposer:** Convert high-level business goals (e.g., *"Clear aging seasonal stock in 21 days at >25% gross margin"*) into time-bound plans, scheduled heartbeats, and delegated tickets.
* **FR-3.2 Pre-Built Specialist Agent Roster (8 Core Roles):**
  1. **Executive Business Planner:** Goal decomposition, scenario simulation, daily briefings.
  2. **Inbound/Outbound Customer Support Voice Agent:** Order tracking, returns, product FAQs.
  3. **Sales & Cart Recovery Voice Agent:** COD verification, abandoned cart callbacks, upsells.
  4. **B2B & Distributor Desk Agent:** Bulk tier quotes, credit limit checks, repeat dealer orders.
  5. **Procurement & Vendor Agent:** Low-stock RFQs, supplier follow-ups, draft POs.
  6. **Finance & Reconciliation Auditor:** Gateway payout matching, Zoho Books/SAP invoicing, overdue reminders.
  7. **Multi-Channel Catalog & Inventory Router:** Buffer sync across Shopify, BigCommerce, Odoo, SAP.
  8. **SOP & Knowledge Sync Agent:** Syncs human overrides/corrections into Notion playbooks automatically.
* **FR-3.3 Heartbeat & Cron Scheduler:** Run proactive background checks (e.g., 15-min stock reconciliation, nightly payout matching) independent of user chat prompts.
* **FR-3.4 Model-Agnostic LLM Router:** Route routine classification/extraction tasks to low-latency, low-cost models and complex multi-step planning to frontier reasoning models.

### 3.4 Module 4: Real-Time Voice & Omnichannel Edge
* **FR-4.1 Telephony & WebRTC Ingestion:** Support inbound/outbound PSTN calls via SIP trunking, browser WebRTC widgets, and WhatsApp voice/messaging.
* **FR-4.2 Talker-Reasoner Split Execution:**
  * **Synchronous Talker:** Reads from a hot operational cache (Redis) to answer live queries (order status, stock availability, pricing) with <800ms Time-to-First-Audio.
  * **Asynchronous Reasoner:** Dispatches multi-ERP mutations (order creation, invoice generation, address edits) to background workers without stalling voice conversation flow.
* **FR-4.3 Live Guardrail Observer & Warm Transfer:** Monitor live call transcripts in parallel for escalation triggers or compliance breaches, seamlessly transferring calls to human agents with full screen-pop context.

### 3.5 Module 5: User Interface & Interaction Screens
* **FR-5.1 Live Operations Execution Board (Home View):** Real-time pipeline lanes (Incoming Orders, Stock Sync, Invoice & Dispatch) showing live webhook events, SLA timers, system health pills, and an active Side Approval Drawer.
* **FR-5.2 Agent Org Chart & Slide-Over Customizer:** Visual hierarchy of active agents with live status, spend meters, and a 4-tab slide-over drawer (Identity, Playbooks, Heartbeats, Guardrails) for no-code customization.
* **FR-5.3 Business Planner & Scenario Canvas:** Interactive goal tracker showing active campaigns, projected vs. actual KPIs, and generated sub-tickets.
* **FR-5.4 Decision X-Ray & "Correct Agent" Modal:** Step-by-step execution trace with an inline correction input that updates agent rules and Notion SOPs automatically.

---

## 4. Non-Functional Requirements (NFRs)

| Category | Requirement | Target SLA / Metric |
| :--- | :--- | :--- |
| **Voice Latency** | End-to-end voice turn latency (User stops speaking $\rightarrow$ First audio byte) | $\le$ 800 ms (P95) |
| **Webhook Ingestion** | Ingestion, deduplication, and cache update from Shopify/Odoo/Zoho | $\le$ 250 ms (P99) |
| **Data Consistency** | Zero duplicate financial writes or negative stock races across ERPs | 100% Idempotent |
| **Security & Auth** | Zero plaintext credentials in LLM prompts or agent memory | OAuth 2.1 / Vault KMS |
| **Availability** | Core event bus, voice edge, and approval queue uptime | 99.95% |
| **Observability** | Traceability of every AI decision and external MCP API call | 100% Audit Coverage |

---

## 5. System Architecture & Data Contracts

### 5.1 Core Canonical Schema (PostgreSQL)
* **`workspaces`:** Multi-tenant isolation, default currency, tax jurisdiction, global spend limits.
* **`mcp_connections`:** Connected server URI, transport type (`http` / `stdio`), encrypted auth ref, health status, allowed scopes.
* **`canonical_entities`:** `id` (UUID), `entity_type` (`sku` | `order` | `customer` | `invoice`), `canonical_payload` (JSONB), `version`, `updated_at`.
* **`external_id_map`:** `canonical_id`, `system_name` (`shopify` | `odoo` | `sap` | `zoho` | `bigcommerce`), `external_id`, `last_synced_at`.
* **`agent_definitions`:** `id`, `role_name`, `model_tier`, `heartbeat_cron`, `daily_budget_usd`, `spent_today_usd`, `status`.
* **`execution_tickets`:** `id`, `goal_id`, `assigned_agent_id`, `status` (`queued` | `running` | `pending_approval` | `conflict_hold` | `completed` | `failed`), `idempotency_key`.
* **`audit_xray_logs`:** `ticket_id`, `step_number`, `tool_name`, `input_sanitized`, `output_sanitized`, `policy_decision`, `token_cost`.

### 5.2 Standard MCP Tool Envelope
Every tool exposed through the Gateway must enforce a uniform contract:

```json
{
  "tool": "books.draft_invoice",
  "idempotency_key": "ord_1023_inv_v1",
  "agent_id": "agt_finance_01",
  "workspace_id": "ws_994",
  "payload": {
    "canonical_order_id": "uuid-991",
    "customer_external_id": "zoho_cust_442",
    "line_items": [
      {
        "sku": "SKU-789",
        "qty": 200,
        "unit_price": 420.00
      }
    ]
  }
}
```

---

## 6. Phased Delivery Roadmap & Success Metrics

### Phase 1: Core Spine & MCP Gateway (Weeks 1–3)
* **Deliverables:** PostgreSQL canonical store, idempotency & policy engine, MCP Gateway with Shopify + Odoo + Zoho Books + Notion adapters, and Live Operations Execution Board with Approval Drawer.
* **Exit Criteria:** End-to-end order-to-draft-invoice sync with zero duplicate writes and 100% Decision X-Ray logging.

### Phase 2: Real-Time Voice Workforce (Weeks 4–6)
* **Deliverables:** LiveKit + SIP telephony pipeline, Redis hot cache sync, Customer Support Voice Agent and Sales/COD Verification Voice Agent, plus live call handoff.
* **Exit Criteria:** P95 voice latency <800ms; successful live order lookup and async order modification during active phone calls.

### Phase 3: AI Business Planner & Enterprise Expansion (Weeks 7–10)
* **Deliverables:** Executive Business Planner (Goal $\rightarrow$ Ticket decomposition), BigCommerce & SAP MCP adapters, Natural-Language Policy Compiler, and bi-directional Notion SOP sync.
* **Exit Criteria:** Autonomous execution of a multi-week inventory clearance plan across 2+ storefronts and 2+ ERPs under strict budget caps.

### Key Product KPIs (North Star Metrics)
* **Touchless Resolution Rate:** % of orders, inquiries, and reconciliations completed without human intervention (Target: >75% within 60 days).
* **Exception Triage Time:** Average time for a human manager to review and approve/override a flagged card (Target: <15 seconds).
* **Time-to-First-Value (TTFV):** Time from connecting first MCP server to live automated workflow execution (Target: <30 minutes).
