"""Single-hop HTTPS transport used by public clients."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
import ssl
from typing import Mapping, Protocol
from urllib.error import HTTPError, URLError
from urllib.request import HTTPRedirectHandler, HTTPSHandler, Request, build_opener


class TransportUnavailable(Exception):
    pass


@dataclass(frozen=True, slots=True)
class Response:
    status: int
    headers: Mapping[str, tuple[str, ...]]
    body: bytes


class AsyncTransport(Protocol):
    async def request(
        self,
        method: str,
        url: str,
        headers: Mapping[str, str],
        body: bytes,
        timeout: float,
    ) -> Response: ...


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # type: ignore[no-untyped-def]
        return None


class HTTPSJSONTransport:
    """urllib transport with system TLS and redirects disabled."""

    def __init__(self, *, ssl_context: ssl.SSLContext | None = None) -> None:
        context = ssl_context or ssl.create_default_context()
        self._opener = build_opener(_NoRedirect(), HTTPSHandler(context=context))

    async def request(
        self,
        method: str,
        url: str,
        headers: Mapping[str, str],
        body: bytes,
        timeout: float,
    ) -> Response:
        return await asyncio.to_thread(
            self._request_sync, method, url, headers, body, timeout
        )

    def _request_sync(
        self,
        method: str,
        url: str,
        headers: Mapping[str, str],
        body: bytes,
        timeout: float,
    ) -> Response:
        request = Request(url, data=body, headers=dict(headers), method=method)
        try:
            opened = self._opener.open(request, timeout=timeout)
        except HTTPError as failure:
            opened = failure
        except (URLError, OSError, TimeoutError):
            raise TransportUnavailable("transport unavailable") from None
        with opened:
            payload = opened.read((8 << 20) + 1)
            if len(payload) > 8 << 20:
                raise TransportUnavailable("response too large")
            response_headers: dict[str, tuple[str, ...]] = {}
            for name in opened.headers:
                response_headers[name.lower()] = tuple(opened.headers.get_all(name) or ())
            return Response(opened.status, response_headers, payload)


def one_header(response: Response, name: str, *, required: bool = True) -> str:
    values = response.headers.get(name.lower(), ())
    if not values and not required:
        return ""
    if len(values) != 1 or not values[0] or values[0].strip() != values[0]:
        raise TransportUnavailable("invalid response header")
    return values[0]
