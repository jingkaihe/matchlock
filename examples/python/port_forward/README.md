# Python SDK Port-Forward Example

Starts `nginx:alpine` in a Matchlock sandbox and demonstrates both styles of forwarding:

- Create-time mapping: host `8080` -> guest `80` via `Sandbox.with_port_forward(8080, 80)`
- Runtime mapping: host `18080` -> guest `80` via `client.port_forward("18080:80")`

## Run

From the repository root:

```bash
uv run examples/python/port_forward/main.py
```

## Notes

- The script polls `http://127.0.0.1:8080` first, then adds runtime forwarding and polls `http://127.0.0.1:18080`.
- If port `8080` or `18080` is already in use on your host, pick different local ports in the script.
- Set `MATCHLOCK_BIN` to point at your local binary if needed, for example:

```bash
MATCHLOCK_BIN=./bin/matchlock uv run --with-editable ./sdk/python examples/python/port_forward/main.py
```
