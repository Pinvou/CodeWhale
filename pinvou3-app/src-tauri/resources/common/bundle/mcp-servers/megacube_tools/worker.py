"""Dedicated transfer entrypoint; deliberately separate from the MCP server."""
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from megacube_tools.transfers import run_worker

if __name__ == "__main__":
    run_worker(sys.argv[1])
