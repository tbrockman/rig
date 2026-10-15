//! The rules, and the rewriting they drive. No WIT here, so every decision is
//! testable on the host.
//!
//! A rule pairs a stand-in the guest was given with the real value and the
//! endpoints it may be sent to. The guest puts the stand-in wherever it would
//! have put the secret; a request to one of the rule's endpoints leaves the
//! proxy with the real value in that place. A request anywhere else leaves
//! with the stand-in, which is worth nothing there.
//!
//! An endpoint is a host, and optionally the methods allowed and a path: the
//! path itself and everything below it. `api.example.com` alone is every
//! request to that host; `POST api.example.com/v1/messages` is one call.
//!
//! Matching is by substring: `Authorization: Bearer <stand-in>` and
//! `x-api-key: <stand-in>` are the same operation. A stand-in that has been
//! encoded (Basic auth's base64) is not found, by design: the plugin never
//! guesses at encodings a secret might be hiding in.

use serde::Deserialize;

/// One place a credential may go.
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct Endpoint {
    /// An exact host name, or `*.example.com` for a host and every
    /// subdomain. Lower-cased on parse.
    pub host: String,
    /// The methods allowed, upper-cased on parse. Empty is any.
    #[serde(default)]
    pub methods: Vec<String>,
    /// The path allowed, and everything below it. Absent is any.
    #[serde(default)]
    pub path: Option<String>,
}

/// One stand-in and what it stands for.
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct Rule {
    /// What the guest holds. Opaque, and long enough that it never occurs
    /// in a request by accident.
    pub placeholder: String,
    /// What goes out in its place.
    pub value: String,
    /// Where the real value may go.
    pub endpoints: Vec<Endpoint>,
}

/// The configured rules, checked once at `create`.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Rules(Vec<Rule>);

impl Rules {
    /// Parses the `rules` setting: a JSON list of rules. Empty input is no
    /// rules, which is a plugin that rewrites nothing.
    pub fn parse(json: &str) -> Result<Rules, String> {
        if json.trim().is_empty() {
            return Ok(Rules::default());
        }
        let mut rules: Vec<Rule> = serde_json::from_str(json)
            .map_err(|e| format!("rules is not a JSON list of rules: {e}"))?;
        for rule in rules.iter_mut() {
            if rule.placeholder.len() < 16 {
                return Err(format!(
                    "placeholder {:?} is too short to be safe as a substring match (16 characters at least)",
                    rule.placeholder
                ));
            }
            if rule.value.is_empty() {
                return Err(format!(
                    "rule for {:?} has an empty value",
                    rule.placeholder
                ));
            }
            if rule.endpoints.is_empty() {
                return Err(format!(
                    "rule for {:?} names no endpoints",
                    rule.placeholder
                ));
            }
            for ep in rule.endpoints.iter_mut() {
                ep.host = ep.host.trim().to_ascii_lowercase();
                let bare = ep.host.strip_prefix("*.").unwrap_or(&ep.host);
                if bare.is_empty() || bare.contains('/') || bare.contains(':') || bare.contains('*')
                {
                    return Err(format!("{:?} is not a host name or *.suffix", ep.host));
                }
                for m in ep.methods.iter_mut() {
                    *m = m.trim().to_ascii_uppercase();
                    if m.is_empty() || !m.bytes().all(|b| b.is_ascii_uppercase()) {
                        return Err(format!("{m:?} is not an HTTP method"));
                    }
                }
                if let Some(p) = ep.path.as_mut() {
                    *p = p.trim().to_string();
                    if !p.starts_with('/') || p.contains('?') || p.contains('#') {
                        return Err(format!("path {p:?} must start with / and carry no query"));
                    }
                    while p.len() > 1 && p.ends_with('/') {
                        p.pop();
                    }
                    if p == "/" {
                        ep.path = None;
                    }
                }
            }
        }
        // One stand-in inside another would make the result depend on the
        // order the rules were applied in.
        for (i, a) in rules.iter().enumerate() {
            for (j, b) in rules.iter().enumerate() {
                if i != j && b.placeholder.contains(&a.placeholder) {
                    return Err(format!(
                        "placeholder {:?} occurs inside {:?}; stand-ins must be distinct",
                        a.placeholder, b.placeholder
                    ));
                }
            }
        }
        Ok(Rules(rules))
    }

    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    /// The rules with an endpoint that covers this request.
    pub fn applicable<'a>(
        &'a self,
        host: &'a str,
        method: &'a str,
        path: &'a str,
    ) -> impl Iterator<Item = &'a Rule> + 'a {
        self.0
            .iter()
            .filter(move |r| r.endpoints.iter().any(|e| e.covers(host, method, path)))
    }

    /// `s` with every stand-in that may go to this request replaced by its
    /// value. `None` when nothing in it changed.
    pub fn rewrite(&self, host: &str, method: &str, path: &str, s: &str) -> Option<String> {
        let mut out = s.to_string();
        let mut changed = false;
        for rule in self.applicable(host, method, path) {
            if out.contains(&rule.placeholder) {
                out = out.replace(&rule.placeholder, &rule.value);
                changed = true;
            }
        }
        changed.then_some(out)
    }
}

impl Endpoint {
    /// Whether this endpoint covers a request: host, method and path all.
    pub fn covers(&self, host: &str, method: &str, path: &str) -> bool {
        host_matches(&self.host, host)
            && (self.methods.is_empty() || self.methods.iter().any(|m| m == method))
            && self.path.as_deref().is_none_or(|p| path_matches(p, path))
    }
}

/// Whether `pattern` (a host, or `*.suffix`) covers `host`. A `*.suffix`
/// covers the suffix itself too: a credential for `*.example.com` is meant
/// for the whole site, and the apex is part of it.
pub fn host_matches(pattern: &str, host: &str) -> bool {
    match pattern.strip_prefix("*.") {
        Some(suffix) => host == suffix || host.ends_with(&format!(".{suffix}")),
        None => pattern == host,
    }
}

/// Whether a granted path covers a request's: the path itself, or anything
/// below it. `/v1/messages` covers `/v1/messages/batches` and not
/// `/v1/messagesx`.
pub fn path_matches(granted: &str, actual: &str) -> bool {
    actual == granted || actual.starts_with(&format!("{granted}/"))
}

/// The host of an authority: lower-cased, without the port, without the
/// brackets of an IPv6 literal, without a trailing dot.
pub fn host_of(authority: &str) -> String {
    let a = authority.trim();
    let host = if let Some(rest) = a.strip_prefix('[') {
        rest.split(']').next().unwrap_or("")
    } else {
        a.rsplit_once(':')
            .filter(|(_, p)| !p.is_empty() && p.chars().all(|c| c.is_ascii_digit()))
            .map(|(h, _)| h)
            .unwrap_or(a)
    };
    host.trim_end_matches('.').to_ascii_lowercase()
}

/// The path of a request target, without its query. None is the root.
pub fn path_of(path_with_query: Option<&str>) -> &str {
    let p = path_with_query.unwrap_or("/");
    let p = p.split('?').next().unwrap_or("/");
    if p.is_empty() { "/" } else { p }
}

#[cfg(test)]
mod tests {
    use super::*;

    const PH: &str = "rig-0123456789abcdef0123456789abcdef";

    fn rules(json: &str) -> Rules {
        Rules::parse(json).expect("valid rules")
    }

    fn one(endpoints: &str) -> Rules {
        rules(&format!(
            r#"[{{"placeholder":"{PH}","value":"sk-real","endpoints":{endpoints}}}]"#
        ))
    }

    #[test]
    fn parses_and_normalises() {
        let r = one(
            r#"[{"host":" API.Anthropic.com ","methods":["post"," Get"],"path":"/v1/messages/"},{"host":"*.Example.com","path":"/"}]"#,
        );
        let eps = &r.0[0].endpoints;
        assert_eq!(eps[0].host, "api.anthropic.com");
        assert_eq!(eps[0].methods, vec!["POST", "GET"]);
        assert_eq!(eps[0].path.as_deref(), Some("/v1/messages"));
        assert_eq!(eps[1].host, "*.example.com");
        assert_eq!(eps[1].path, None, "the root path is no restriction");
        assert!(Rules::parse("").unwrap().is_empty());
        assert!(Rules::parse("   ").unwrap().is_empty());
    }

    #[test]
    fn refuses_what_would_misbehave() {
        let short = r#"[{"placeholder":"abc","value":"v","endpoints":[{"host":"a.com"}]}]"#;
        assert!(Rules::parse(short).unwrap_err().contains("too short"));
        let none = format!(r#"[{{"placeholder":"{PH}","value":"v","endpoints":[]}}]"#);
        assert!(Rules::parse(&none).unwrap_err().contains("no endpoints"));
        let empty = format!(r#"[{{"placeholder":"{PH}","value":"","endpoints":[{{"host":"a.com"}}]}}]"#);
        assert!(Rules::parse(&empty).unwrap_err().contains("empty value"));
        let bad_host =
            format!(r#"[{{"placeholder":"{PH}","value":"v","endpoints":[{{"host":"https://a.com"}}]}}]"#);
        assert!(Rules::parse(&bad_host).unwrap_err().contains("not a host name"));
        let bad_method = format!(
            r#"[{{"placeholder":"{PH}","value":"v","endpoints":[{{"host":"a.com","methods":["p0st"]}}]}}]"#
        );
        assert!(Rules::parse(&bad_method).unwrap_err().contains("not an HTTP method"));
        let bad_path = format!(
            r#"[{{"placeholder":"{PH}","value":"v","endpoints":[{{"host":"a.com","path":"v1?x=1"}}]}}]"#
        );
        assert!(Rules::parse(&bad_path).unwrap_err().contains("must start with /"));
        let nested = format!(
            r#"[{{"placeholder":"{PH}","value":"v","endpoints":[{{"host":"a.com"}}]}},{{"placeholder":"{PH}-more","value":"w","endpoints":[{{"host":"b.com"}}]}}]"#
        );
        assert!(Rules::parse(&nested).unwrap_err().contains("occurs inside"));
        assert!(Rules::parse("{}").is_err());
        // The old shape, hosts without endpoints, is refused rather than
        // read as a rule that covers nothing.
        let old = format!(r#"[{{"placeholder":"{PH}","value":"v","hosts":["a.com"]}}]"#);
        assert!(Rules::parse(&old).is_err());
    }

    #[test]
    fn rewrites_only_for_the_rules_endpoints() {
        let r = one(r#"[{"host":"api.anthropic.com"}]"#);
        let bearer = format!("Bearer {PH}");
        assert_eq!(
            r.rewrite("api.anthropic.com", "POST", "/v1/messages", &bearer)
                .as_deref(),
            Some("Bearer sk-real")
        );
        // Elsewhere the stand-in goes out as it is, worth nothing.
        assert_eq!(r.rewrite("evil.example", "POST", "/", &bearer), None);
        // Nothing to do is reported as nothing, not as an identical copy.
        assert_eq!(r.rewrite("api.anthropic.com", "POST", "/", "Bearer other"), None);
    }

    #[test]
    fn methods_and_paths_narrow_an_endpoint() {
        let r = one(r#"[{"host":"api.anthropic.com","methods":["POST"],"path":"/v1/messages"}]"#);
        let bearer = format!("Bearer {PH}");
        assert!(r.rewrite("api.anthropic.com", "POST", "/v1/messages", &bearer).is_some());
        assert!(r.rewrite("api.anthropic.com", "POST", "/v1/messages/batches", &bearer).is_some());
        assert!(r.rewrite("api.anthropic.com", "GET", "/v1/messages", &bearer).is_none());
        assert!(r.rewrite("api.anthropic.com", "POST", "/v1/models", &bearer).is_none());
        assert!(r.rewrite("api.anthropic.com", "POST", "/v1/messagesx", &bearer).is_none());
        assert!(r.rewrite("api.anthropic.com", "POST", "/", &bearer).is_none());
    }

    #[test]
    fn several_endpoints_are_a_union() {
        let r = one(
            r#"[{"host":"a.com","methods":["GET"]},{"host":"a.com","methods":["POST"],"path":"/x"}]"#,
        );
        let s = format!("{PH}");
        assert!(r.rewrite("a.com", "GET", "/anything", &s).is_some());
        assert!(r.rewrite("a.com", "POST", "/x/y", &s).is_some());
        assert!(r.rewrite("a.com", "POST", "/y", &s).is_none());
    }

    #[test]
    fn wildcard_hosts_cover_the_apex_and_subdomains() {
        assert!(host_matches("*.example.com", "example.com"));
        assert!(host_matches("*.example.com", "api.example.com"));
        assert!(host_matches("*.example.com", "a.b.example.com"));
        assert!(!host_matches("*.example.com", "notexample.com"));
        assert!(!host_matches("example.com", "api.example.com"));
    }

    #[test]
    fn host_of_strips_ports_brackets_and_case() {
        assert_eq!(host_of("API.Anthropic.com:443"), "api.anthropic.com");
        assert_eq!(host_of("api.anthropic.com"), "api.anthropic.com");
        assert_eq!(host_of("api.anthropic.com."), "api.anthropic.com");
        assert_eq!(host_of("[::1]:8443"), "::1");
        assert_eq!(host_of("[2001:db8::1]"), "2001:db8::1");
    }

    #[test]
    fn path_of_drops_the_query() {
        assert_eq!(path_of(Some("/v1/messages?beta=1")), "/v1/messages");
        assert_eq!(path_of(Some("/")), "/");
        assert_eq!(path_of(Some("")), "/");
        assert_eq!(path_of(None), "/");
    }

    #[test]
    fn two_rules_two_hosts() {
        let r = rules(&format!(
            r#"[{{"placeholder":"{PH}","value":"A","endpoints":[{{"host":"a.com"}}]}},{{"placeholder":"rig-ffffffffffffffffffffffffffffffff","value":"B","endpoints":[{{"host":"b.com"}}]}}]"#
        ));
        let both = format!("{PH} rig-ffffffffffffffffffffffffffffffff");
        assert_eq!(
            r.rewrite("a.com", "GET", "/", &both).as_deref(),
            Some("A rig-ffffffffffffffffffffffffffffffff")
        );
        assert_eq!(
            r.rewrite("b.com", "GET", "/", &both).as_deref(),
            Some(&*format!("{PH} B"))
        );
    }
}
