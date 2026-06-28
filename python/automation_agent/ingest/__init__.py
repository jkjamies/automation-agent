"""ingest — the normalized Envelope and Kind enum for all ingress sources."""

from automation_agent.ingest.envelope import Envelope, Kind, decode, encode, new

__all__ = ["Envelope", "Kind", "decode", "encode", "new"]
