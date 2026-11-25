import os

# Tests run in dev mode: fixtures and mock LLM are allowed. Production-mode behaviour is
# tested explicitly by monkeypatching ABOS_ENV.
os.environ.setdefault("ABOS_ENV", "dev")
# Never talk to real services from unit tests.
for _k in ("GEMINI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"):
    os.environ.pop(_k, None)
os.environ["REDIS_URL"] = "redis://127.0.0.1:1/0"  # unreachable on purpose => dev fixtures
