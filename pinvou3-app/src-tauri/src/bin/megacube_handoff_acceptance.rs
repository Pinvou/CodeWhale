//! Opt-in local acceptance host; never part of the default desktop build.
fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args == ["--cmsd-store-inventory"] {
        pinvou3_lib::run();
        return;
    }
    if let Err(error) = pinvou3_lib::run_megacube_handoff_acceptance() {
        eprintln!("handoff acceptance failed: {error:#}");
        std::process::exit(1);
    }
}
