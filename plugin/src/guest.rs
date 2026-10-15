//! The component: WIT bindings, the manifest, and the request rewrite.

use crate::rules::{Rules, host_of, path_of};

wit_bindgen::generate!({
    world: "witmproxy:plugin/plugin",
    path: "wit",
    generate_all
});

use self::ezco::ezcap::types::Scope;
use exports::witmproxy::plugin::witm_plugin::{
    ActualInput, Capability, CapabilityProvider, ConfigureError, Guest, GuestPlugin, InputSchema,
    InputType, Plugin as PluginResource, PluginError, PluginManifest, UserInput,
};
use wasi::http::types::{Method, Scheme};
use witmproxy::plugin::capabilities::{CapabilityKind, Event, EventKind, Request};

const PUBLIC_KEY_BYTES: &[u8] = include_bytes!("../key.public");

/// The one setting: the rules, as JSON. A `secret`, so the store encrypts it
/// and no listing or API ever echoes it: it holds the real values.
const RULES: &str = "rules";

struct Component;

impl Guest for Component {
    type Plugin = PluginInstance;

    async fn manifest() -> PluginManifest {
        PluginManifest {
            name: "rig".to_string(),
            namespace: "joinezco".to_string(),
            author: "Theodore Brockman".to_string(),
            version: env!("CARGO_PKG_VERSION").to_string(),
            description: env!("CARGO_PKG_DESCRIPTION").to_string(),
            metadata: vec![],
            // Every connection of a tenant this plugin is enabled for is
            // terminated here, and every request of theirs passes through:
            // the plugin hands back unchanged whatever no rule applies to.
            // rig enables it per guest, never globally, so a client the
            // proxy cannot name is never intercepted on its account.
            capabilities: vec![
                Capability {
                    kind: CapabilityKind::HandleEvent(EventKind::Connect),
                    scope: Scope {
                        when: "true".into(),
                        allow: "true".into(),
                    },
                },
                Capability {
                    kind: CapabilityKind::HandleEvent(EventKind::Request),
                    scope: Scope {
                        when: "true".into(),
                        allow: "true".into(),
                    },
                },
            ],
            license: env!("CARGO_PKG_LICENSE").to_string(),
            url: env!("CARGO_PKG_REPOSITORY").to_string(),
            publickey: PUBLIC_KEY_BYTES.to_vec(),
            configuration: vec![InputSchema {
                name: RULES.to_string(),
                input_type: InputType::Secret,
                optional: true,
                default: None,
                description: Some(
                    "A JSON list of rules: [{\"placeholder\": \"<what the guest holds, 16+ chars>\", \
                     \"value\": \"<the real credential>\", \"endpoints\": [{\"host\": \"api.example.com\", \
                     \"methods\": [\"POST\"], \"path\": \"/v1/messages\"}]}]. In an HTTPS request to one \
                     of a rule's endpoints (methods and path optional), the placeholder is replaced by \
                     the value wherever it occurs in a header value or the URL."
                        .to_string(),
                ),
            }],
        }
    }
}

struct PluginInstance {
    rules: Rules,
}

impl GuestPlugin for PluginInstance {
    async fn create(config: Vec<UserInput>) -> Result<PluginResource, ConfigureError> {
        let mut rules = Rules::default();
        for input in config {
            if input.name != RULES {
                continue;
            }
            let json = match input.value {
                // The store holds what it is given as the declared type; a
                // tenant's value arrives as a plain string.
                ActualInput::Secret(s) | ActualInput::Str(s) => s,
                _ => {
                    return Err(ConfigureError::InvalidInputs(vec![]));
                }
            };
            rules = Rules::parse(&json).map_err(ConfigureError::Other)?;
        }
        Ok(PluginResource::new(PluginInstance { rules }))
    }

    async fn handle(
        &self,
        ev: Event,
        _cp: CapabilityProvider,
    ) -> Result<Option<Event>, PluginError> {
        match ev {
            Event::Request(req) => Ok(Some(Event::Request(self.rewrite(req)))),
            other => Ok(Some(other)),
        }
    }
}

/// The method as the rules spell it.
fn method_name(m: &Method) -> String {
    match m {
        Method::Get => "GET".into(),
        Method::Head => "HEAD".into(),
        Method::Post => "POST".into(),
        Method::Put => "PUT".into(),
        Method::Delete => "DELETE".into(),
        Method::Connect => "CONNECT".into(),
        Method::Options => "OPTIONS".into(),
        Method::Trace => "TRACE".into(),
        Method::Patch => "PATCH".into(),
        Method::Other(s) => s.to_ascii_uppercase(),
    }
}

impl PluginInstance {
    /// The request with every stand-in that may go to it replaced. A
    /// request nothing applies to is handed back as it is, untouched.
    fn rewrite(&self, req: Request) -> Request {
        if self.rules.is_empty() {
            return req;
        }
        // Only into a request that leaves encrypted. A credential put into
        // a plaintext request is a credential on the wire; a client asking
        // for that gets the stand-in, and a failure it can read.
        let scheme = req.get_scheme();
        if !matches!(scheme, Some(Scheme::Https)) {
            return req;
        }
        let authority = req.get_authority();
        let host = host_of(authority.as_deref().unwrap_or(""));
        let method = req.get_method();
        let method = method_name(&method);
        let path_with_query = req.get_path_with_query();
        let path = path_of(path_with_query.as_deref()).to_string();
        if host.is_empty()
            || self
                .rules
                .applicable(&host, &method, &path)
                .next()
                .is_none()
        {
            return req;
        }

        // Headers: grouped by name, because `set` replaces every value of a
        // name at once and a header may legitimately repeat.
        let old_headers = req.get_headers();
        let mut grouped: Vec<(String, Vec<Vec<u8>>, bool)> = Vec::new();
        for (name, value) in old_headers.copy_all() {
            let (value, changed) = match std::str::from_utf8(&value) {
                Ok(s) => match self.rules.rewrite(&host, &method, &path, s) {
                    Some(new) => (new.into_bytes(), true),
                    None => (value, false),
                },
                Err(_) => (value, false),
            };
            match grouped.iter_mut().find(|(n, _, _)| *n == name) {
                Some(entry) => {
                    entry.1.push(value);
                    entry.2 |= changed;
                }
                None => grouped.push((name, vec![value], changed)),
            }
        }
        let new_path = path_with_query
            .as_deref()
            .and_then(|p| self.rules.rewrite(&host, &method, &path, p));

        if new_path.is_none() && !grouped.iter().any(|(_, _, changed)| *changed) {
            return req;
        }

        let headers = old_headers.clone();
        for (name, values, changed) in &grouped {
            if *changed {
                // A value the client sent was a valid header value; the real
                // one replacing a stand-in inside it still is, unless the
                // operator configured a value no header may carry, in which
                // case the stand-in goes out and the request fails upstream,
                // which is the legible failure.
                let _ = headers.set(name, values);
            }
        }

        let method = req.get_method();
        let (_, result_rx) = wit_future::new(|| Ok(()));
        let (body, trailers) = Request::consume_body(req, result_rx);
        let (new_req, _) = Request::new(headers, Some(body), trailers, None);
        let _ = new_req.set_method(&method);
        let _ = new_req.set_authority(authority.as_deref());
        let _ = new_req.set_scheme(scheme.as_ref());
        let _ = new_req.set_path_with_query(new_path.as_deref().or(path_with_query.as_deref()));
        new_req
    }
}

export!(Component);
