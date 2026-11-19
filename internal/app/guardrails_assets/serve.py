"""Run the NeMo Guardrails check server on loopback.

The nemoguardrails CLI binds 0.0.0.0 and has no host option. This runner sets
the same attributes the CLI sets, then binds 127.0.0.1 so the check endpoint is
reachable only from this machine.
"""

import logging
import os

import uvicorn

from nemoguardrails.server import api

# The library logs checked text at DEBUG and INFO. orr must not persist prompts
# or response bodies, so keep the sidecar at WARNING and above.
logging.getLogger("nemoguardrails").setLevel(logging.WARNING)

api.app.rails_config_path = os.environ["ORR_GUARDRAILS_CONFIG"]
api.set_default_config_id(os.environ.get("ORR_GUARDRAILS_CONFIG_ID", "orr"))

uvicorn.run(
    api.app,
    host="127.0.0.1",
    port=int(os.environ.get("ORR_GUARDRAILS_PORT", "8765")),
    log_level=os.environ.get("ORR_GUARDRAILS_LOG_LEVEL", "warning"),
)
