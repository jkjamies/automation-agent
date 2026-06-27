"""Tests for GitHub App config resolution and mode selection.

Mirrors ``go/internal/config/config_app_test.go``: the env-var contract, the
App-vs-PAT mode selection, positive-id and RSA-PEM validation, the flattened-``\\n``
unescape (including the trailing-newline regression), and the empty-REPOS rejection.
No network, no LLM — throwaway keys generated in-process.
"""

from __future__ import annotations

import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ec, rsa

from automation_agent.config import load_from


def map_lookup(m: dict[str, str]):
    return m.get


def _rsa_pem(pkcs1: bool = False) -> str:
    """A throwaway RSA private key in PEM (PKCS#8 by default, PKCS#1 when requested)."""
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    fmt = serialization.PrivateFormat.TraditionalOpenSSL if pkcs1 else serialization.PrivateFormat.PKCS8
    return key.private_bytes(
        encoding=serialization.Encoding.PEM,
        format=fmt,
        encryption_algorithm=serialization.NoEncryption(),
    ).decode()


def _ec_pem() -> str:
    """A throwaway EC (non-RSA) private key in PKCS#8 PEM — must be rejected."""
    key = ec.generate_private_key(ec.SECP256R1())
    return key.private_bytes(
        encoding=serialization.Encoding.PEM,
        format=serialization.PrivateFormat.PKCS8,
        encryption_algorithm=serialization.NoEncryption(),
    ).decode()


# A valid RSA key shared across the cases that don't probe key parsing (keygen is slow).
_PEM = _rsa_pem()


def _app_env(overrides: dict[str, str]) -> dict[str, str]:
    """The full set of vars that select App mode, with overrides merged in. REPOS is
    included because App mode rejects an empty allow-list."""
    base = {
        "GITHUB_APP_ID": "42",
        "GITHUB_APP_INSTALLATION_ID": "99",
        "GITHUB_APP_PRIVATE_KEY": _PEM,
        "REPOS": "acme/api",
    }
    base.update(overrides)
    return base


def test_pat_mode_when_no_app_vars() -> None:
    c = load_from(map_lookup({"GITHUB_TOKEN": "pat", "REPOS": "acme/api"}))
    assert not c.app_mode()
    assert c.github_app_id == 0
    assert c.github_app_installation_id == 0
    assert c.github_app_private_key_pem == b""


def test_app_mode_full_set() -> None:
    c = load_from(map_lookup(_app_env({})))
    assert c.app_mode()
    assert c.github_app_id == 42
    assert c.github_app_installation_id == 99
    assert b"-----BEGIN" in c.github_app_private_key_pem


def test_app_mode_pkcs1_key() -> None:
    c = load_from(map_lookup(_app_env({"GITHUB_APP_PRIVATE_KEY": _rsa_pem(pkcs1=True)})))
    assert c.app_mode()


def test_app_mode_key_from_file(tmp_path) -> None:
    key_file = tmp_path / "app.pem"
    key_file.write_text(_PEM)
    c = load_from(
        map_lookup(
            _app_env(
                {"GITHUB_APP_PRIVATE_KEY": "", "GITHUB_APP_PRIVATE_KEY_PATH": str(key_file)}
            )
        )
    )
    assert c.app_mode()
    assert b"-----BEGIN" in c.github_app_private_key_pem


def test_app_mode_flattened_key_unescaped() -> None:
    flattened = _PEM.replace("\n", "\\n")
    c = load_from(map_lookup(_app_env({"GITHUB_APP_PRIVATE_KEY": flattened})))
    assert c.app_mode()
    assert b"\\n" not in c.github_app_private_key_pem  # escaped \n restored


def test_app_mode_flattened_key_with_trailing_newline_from_file(tmp_path) -> None:
    # A secret store can flatten newlines to literal `\n` and still append one real
    # trailing newline; the unescape must run on the escaped sequences regardless. The
    # file path is read untrimmed, so this exercises the corrected condition directly.
    key_file = tmp_path / "flat.pem"
    key_file.write_text(_PEM.replace("\n", "\\n") + "\n")
    c = load_from(
        map_lookup(
            _app_env(
                {"GITHUB_APP_PRIVATE_KEY": "", "GITHUB_APP_PRIVATE_KEY_PATH": str(key_file)}
            )
        )
    )
    assert c.app_mode()
    assert b"\\n" not in c.github_app_private_key_pem


@pytest.mark.parametrize(
    "overrides",
    [
        pytest.param({"GITHUB_APP_ID": ""}, id="missing app id"),
        pytest.param({"GITHUB_APP_INSTALLATION_ID": ""}, id="missing installation"),
        pytest.param({"GITHUB_APP_PRIVATE_KEY": ""}, id="missing key"),
        pytest.param(
            {"GITHUB_APP_PRIVATE_KEY_PATH": "/some/key.pem"}, id="both key sources"
        ),
        pytest.param({"GITHUB_APP_ID": "0"}, id="zero app id"),
        pytest.param({"GITHUB_APP_ID": "-1"}, id="negative app id"),
        pytest.param({"GITHUB_APP_ID": "abc"}, id="non-numeric app id"),
        pytest.param({"GITHUB_APP_INSTALLATION_ID": "0"}, id="zero installation"),
        pytest.param({"GITHUB_APP_INSTALLATION_ID": "x"}, id="non-numeric installation"),
        pytest.param({"GITHUB_APP_PRIVATE_KEY": "not a pem"}, id="invalid pem"),
        pytest.param({"REPOS": ""}, id="empty repos in app mode"),
    ],
)
def test_app_mode_errors(overrides: dict[str, str]) -> None:
    with pytest.raises(ValueError):
        load_from(map_lookup(_app_env(overrides)))


def test_app_mode_rejects_non_rsa_key() -> None:
    with pytest.raises(ValueError, match="RSA"):
        load_from(map_lookup(_app_env({"GITHUB_APP_PRIVATE_KEY": _ec_pem()})))


def test_app_mode_unreadable_key_file() -> None:
    with pytest.raises(ValueError, match="read GITHUB_APP_PRIVATE_KEY_PATH"):
        load_from(
            map_lookup(
                _app_env(
                    {
                        "GITHUB_APP_PRIVATE_KEY": "",
                        "GITHUB_APP_PRIVATE_KEY_PATH": "/no/such/key.pem",
                    }
                )
            )
        )


def test_repr_redacts_secrets() -> None:
    cfg = load_from(
        map_lookup(
            _app_env(
                {
                    "GITHUB_TOKEN": "ghp_supersecretpat",
                    "GITHUB_WEBHOOK_SECRET": "webhook-shhh",
                    "INTERNAL_TOKEN": "internal-shhh",
                    "SLACK_WEBHOOK_URL": "https://hooks.slack.com/services/SECRETPATH",
                }
            )
        )
    )
    rendered = repr(cfg)
    for leak in ("ghp_supersecretpat", "webhook-shhh", "internal-shhh", "SECRETPATH", "PRIVATE KEY"):
        assert leak not in rendered, f"repr leaked {leak!r}: {rendered}"
    assert "***" in rendered
    assert "github_app_id=42" in rendered  # non-secret fields stay visible for debugging
