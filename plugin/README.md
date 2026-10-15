# @joinezco/rig

The [witmproxy](https://witmproxy.rs) plugin behind rig's
`guest.proxy.credentials`: a guest holds a stand-in, the proxy holds the real
value, and the real value goes out only in requests to the endpoints the
manifest grants.

The guest is given an opaque stand-in where it expects a credential. The
proxy is given the stand-in, the real value, and the endpoints the value may
be sent to. An HTTPS request to one of those endpoints leaves the proxy with
the real value wherever the stand-in occurred in a header value or the URL; a
request anywhere else leaves with the stand-in, which nothing accepts. A
guest that leaks everything it has leaks a string that works only through
this proxy, only there.

rig installs it into each guest's own witmproxy instance, with that guest's
rules as its configuration (`PUT /api/plugins/joinezco/rig/config`). The
plugin itself is not rig-specific: anything that can give a client a
stand-in and the proxy a rule can use it, for one tenant or for everyone.

## Building

The signed component is committed at `../internal/proxy/plugin/rig.signed.wasm`
and embedded in rig, so building rig needs none of this. To change the plugin:

```sh
rustup target add wasm32-wasip2
make            # builds, signs with key.secret, writes the embedded copy
make test       # the rules, on the host
```

`key.secret` is not in git. The public half is compiled into the component,
so a component signed with a different key is a different plugin to
witmproxy; a release should be signed with the key whose `key.public` is
here.

The WIT package under `wit/` is a copy of witmproxy's `witmproxy:plugin`
world at the version this plugin targets.

## Configuration

One setting, `rules`, a `secret` holding a JSON list:

```json
[
  {
    "placeholder": "rig-0123456789abcdef0123456789abcdef",
    "value": "sk-ant-...",
    "endpoints": [
      {"host": "api.anthropic.com", "methods": ["POST"], "path": "/v1/messages"}
    ]
  }
]
```

| Field | |
|---|---|
| `placeholder` | What the client holds. 16 characters at least, so a substring match cannot fire by accident. |
| `value` | What goes out in its place. |
| `endpoints[].host` | An exact host name, or `*.example.com` for a host and all its subdomains. |
| `endpoints[].methods` | Optional. The methods allowed; any when absent. |
| `endpoints[].path` | Optional. The path allowed, and everything below it (`/v1/messages` covers `/v1/messages/batches`); any when absent. |

Only HTTPS requests are rewritten: a credential put into a plaintext request
is a credential on the wire. Matching is by substring, so `Authorization:
Bearer <stand-in>` and `x-api-key: <stand-in>` are the same operation, and a
stand-in that has been encoded (Basic auth's base64) is not found, by design.
