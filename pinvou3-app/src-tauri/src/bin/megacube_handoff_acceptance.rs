//! Opt-in local acceptance host; never part of the default desktop build.
fn main() {
    if let Err(error) = pinvou3_lib::run_megacube_handoff_acceptance() {
        eprintln!("handoff acceptance failed: {error:#}");
        std::process::exit(1);
    }
}
