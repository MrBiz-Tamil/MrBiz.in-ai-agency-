"""Process-wide configuration read from the environment.

ABOS_ENV defaults to "production" so that a missing variable can never enable the
development-only simulations (mock LLM, canned data, fake audio).
"""
import os

# Workspace used by development fixtures and tests (workspace ids are UUIDs everywhere).
DEV_WORKSPACE_ID = "6f1c2c1e-1b0a-4d7e-9a34-0c2a5f2b7e11"


def is_dev() -> bool:
    return os.getenv("ABOS_ENV", "production").strip().lower() == "dev"
