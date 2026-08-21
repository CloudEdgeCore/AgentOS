//! Fixture component construction for the Wasmtime provider.
//!
//! The conformance component implements the agent world
//! (`runtime/wasm/wit/agent.wit`) by echoing its bounded JSON input. It is
//! produced from a hand-written core module (`fixture/agent.wat`) by the
//! `wit-component` adapter encoder, so no external wasm toolchain is required
//! and the workspace stays `--locked`-clean.

use anyhow::{Context, Result};
use std::path::Path;

/// The WIT world path is resolved against the crate manifest so that both the
/// `component-fixture` binary and the test suite work from any working
/// directory.
pub fn wit_path() -> std::path::PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("wit/agent.wit")
}

/// Core module source implementing the canonical ABI of the agent world.
pub const AGENT_WAT: &str = include_str!("../fixture/agent.wat");

/// Encodes the agent world component from the embedded core module.
pub fn build_agent_component() -> Result<Vec<u8>> {
    let mut module = wat::parse_str(AGENT_WAT).context("parse fixture WAT")?;
    let mut resolve = wit_parser::Resolve::new();
    let (package, _source_map) = resolve
        .push_path(wit_path())
        .context("load agent WIT world")?;
    let world = resolve
        .select_world(&[package], Some("agent"))
        .map_err(|error| anyhow::anyhow!("select agent world: {error}"))?;
    wit_component::embed_component_metadata(
        &mut module,
        &resolve,
        world,
        wit_component::StringEncoding::UTF8,
    )
    .context("embed WIT metadata")?;
    let component = wit_component::ComponentEncoder::default()
        .module(&module)
        .context("encode core module into component")?
        .validate(true)
        .encode()
        .context("encode component")?;
    Ok(component)
}
