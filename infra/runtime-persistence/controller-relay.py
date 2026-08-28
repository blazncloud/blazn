#!/usr/bin/env python3
import asyncio
import ipaddress
import os
import ssl
import stat
import sys

BIND = os.environ.get("BLAZN_CONTROLLER_RELAY_BIND", "192.168.0.100")
CERT = "/etc/blazn/control-plane/secrets/object-relay-tls.crt"
KEY = "/etc/blazn/control-plane/secrets/object-relay-tls.key"
ROUTES = (
    (5432, "127.0.0.1", 55432, False),
    (9000, "127.0.0.1", 59000, True),
)


def validate_configuration():
    address = ipaddress.ip_address(BIND)
    if not isinstance(address, ipaddress.IPv4Address) or address.is_loopback or address.is_unspecified:
        raise RuntimeError("controller relay requires one exact non-loopback IPv4 address")
    for path in (CERT, KEY):
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1:
            raise RuntimeError("controller relay TLS material has an unsafe file identity")


async def pipe(reader, writer):
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.CancelledError):
        pass
    finally:
        writer.close()
        try:
            await writer.wait_closed()
        except ConnectionError:
            pass


async def handle(local_reader, local_writer, destination_host, destination_port):
    try:
        remote_reader, remote_writer = await asyncio.open_connection(destination_host, destination_port)
    except (ConnectionError, OSError):
        local_writer.close()
        await local_writer.wait_closed()
        return
    await asyncio.gather(pipe(local_reader, remote_writer), pipe(remote_reader, local_writer))


async def main():
    validate_configuration()
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.minimum_version = ssl.TLSVersion.TLSv1_2
    tls.load_cert_chain(CERT, KEY)
    servers = []
    for listen_port, destination_host, destination_port, terminate_tls in ROUTES:
        server = await asyncio.start_server(
            lambda reader, writer, host=destination_host, port=destination_port: handle(reader, writer, host, port),
            BIND,
            listen_port,
            ssl=tls if terminate_tls else None,
        )
        servers.append(server)
        sys.stderr.write(
            f"controller relay {BIND}:{listen_port} -> {destination_host}:{destination_port}"
            f"{' (tls)' if terminate_tls else ''}\n"
        )
    await asyncio.gather(*(server.serve_forever() for server in servers))


if __name__ == "__main__":
    asyncio.run(main())
