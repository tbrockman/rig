//! `@joinezco/rig`: the witmproxy plugin that gives a rig guest credentials
//! it never holds.
//!
//! The guest is given an opaque stand-in where it expects a credential. The
//! proxy is given the stand-in, the real value, and the endpoints the value
//! may be sent to: a host, and optionally the methods and the path below
//! which it may go. A request to one of those endpoints leaves with the real
//! value in its headers and URL; a request anywhere else leaves with the
//! stand-in, which nothing accepts. A guest that leaks everything it has
//! leaks a string that works only through this proxy, only there.
//!
//! The decisions live in [`rules`] and are unit-tested on the host; [`guest`]
//! is the layer that speaks WIT.

pub mod rules;

#[cfg(target_arch = "wasm32")]
mod guest;
