"""Client of rb-backend (users API)."""

import os

import requests

BACKEND_URL = os.environ.get("BACKEND_URL", "http://rb-backend:8080")


class UserNotFound(Exception):
    pass


def fetch_user(user_id: str) -> dict:
    """GET /api/v1/users/{id} of rb-backend."""
    resp = requests.get(f"{BACKEND_URL}/api/v1/users/{user_id}", timeout=5)
    if resp.status_code == 404:
        raise UserNotFound(user_id)
    resp.raise_for_status()
    return resp.json()
