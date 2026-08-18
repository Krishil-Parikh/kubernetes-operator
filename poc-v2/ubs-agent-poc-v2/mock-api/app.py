"""
Mock UBS Blueprint API + Graph API server.

This simulates the two "internal to UBS" systems the design depends on:

  1. Blueprint API   - source of truth for "which agent ID is currently
                        registered against blueprint X". Plain REST,
                        no locking (as confirmed by the platform team).
                          GET    /blueprint/<id>              -> current registration
                          POST   /blueprint/<id>/register      -> register an agent id
                          POST   /blueprint/<id>/deregister    -> remove an agent id

  2. Graph API       - mints new, globally-unique agent IDs.
                          POST   /graph/agents                 -> {"agentId": "..."}

State is kept in-memory only (dict), reset on restart -- this is a POC,
not a real system. In-cluster it runs as a single-replica Deployment
with a ClusterIP Service so the controller can reach it at a stable
DNS name (e.g. http://mock-ubs-api.ubs-poc.svc.cluster.local).
"""
from flask import Flask, request, jsonify
import itertools
import logging
import threading
import time

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s mock-api %(message)s")
log = logging.getLogger("mock-api")

app = Flask(__name__)

_lock = threading.Lock()
_blueprints = {}          # blueprintId -> {"agentId": str, "image": str}
_retired_agents = set()   # agentIds that have been explicitly deregistered
_agent_id_seq = itertools.count(100)   # deterministic, easy-to-read IDs for demo


@app.route("/graph/agents", methods=["POST"])
def mint_agent_id():
    """Simulates the Graph API minting a brand new agent ID."""
    auth = request.headers.get("Authorization", "")
    log.info("Mint request received, auth header present=%s", bool(auth))
    with _lock:
        new_id = str(next(_agent_id_seq))
    log.info("Minted new agentId=%s", new_id)
    # Simulate realistic network latency
    time.sleep(0.2)
    return jsonify({"agentId": new_id}), 201


@app.route("/blueprint/<blueprint_id>", methods=["GET"])
def get_blueprint(blueprint_id):
    """Returns the currently registered agent for a blueprint, or 404 if none."""
    with _lock:
        entry = _blueprints.get(blueprint_id)
    if entry is None:
        log.info("Blueprint %s has no registered agent", blueprint_id)
        return jsonify({"blueprintId": blueprint_id, "agentId": None}), 200
    log.info("Blueprint %s currently registered agentId=%s image=%s",
              blueprint_id, entry["agentId"], entry.get("image"))
    return jsonify({"blueprintId": blueprint_id, **entry}), 200


@app.route("/blueprint/<blueprint_id>/register", methods=["POST"])
def register_agent(blueprint_id):
    """
    Registers an agent id as the active agent for a blueprint.
    Confirmed NOT to have optimistic locking -- last write wins, by design
    of the real UBS system. The controller is responsible for not calling
    this concurrently for the same blueprint (single-writer / serialized
    reconcile per blueprint).
    """
    body = request.get_json(force=True) or {}
    agent_id = body.get("agentId")
    image = body.get("image", "unknown")
    if not agent_id:
        return jsonify({"error": "agentId required"}), 400

    with _lock:
        _blueprints[blueprint_id] = {"agentId": agent_id, "image": image}
    log.info("Registered agentId=%s (image=%s) against blueprint=%s", agent_id, image, blueprint_id)
    time.sleep(0.2)
    return jsonify({"status": "registered", "blueprintId": blueprint_id, "agentId": agent_id}), 200


@app.route("/agents/<agent_id>/deregister", methods=["POST"])
def deregister_agent(agent_id):
    """
    Retires a specific agent ID. This is intentionally NOT scoped to "is this
    still the current agent for some blueprint" -- per the platform team,
    register and deregister are fully separate calls with no shared
    transaction, so this endpoint just idempotently marks an agent ID as
    retired. It does not touch the blueprint's "current" pointer -- that is
    only ever changed by /register, which the controller calls for the NEW
    agent BEFORE this deregister call ever fires for the OLD one.
    """
    with _lock:
        _retired_agents.add(agent_id)
    log.info("Deregistered (retired) agentId=%s", agent_id)
    time.sleep(0.2)
    return jsonify({"status": "deregistered", "agentId": agent_id}), 200


@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok"}), 200


@app.route("/_debug/state", methods=["GET"])
def debug_state():
    """Not a real UBS endpoint -- POC-only helper to inspect state while demoing."""
    with _lock:
        return jsonify({
            "blueprints": _blueprints,
            "retiredAgents": sorted(_retired_agents),
        }), 200


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5000)
