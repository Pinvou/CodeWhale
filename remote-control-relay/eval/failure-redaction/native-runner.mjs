// Compile the production module directly, using only dependencies already used by the app.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
export const rustPath = join(
  root,
  "pinvou3-app/src-tauri/src/features/behavior_telemetry/diagnostic_redaction.rs",
);
export function nativeRedact(records, work) {
  const runner = join(work, "native-runner");
  mkdirSync(join(runner, "src"), { recursive: true });
  writeFileSync(
    join(runner, "Cargo.toml"),
    '[package]\nname="diagnostic-redaction-eval"\nversion="0.0.0"\nedition="2024"\n[workspace]\n[dependencies]\nserde_json="1"\nsha2="0.11"\n',
  );
  writeFileSync(
    join(runner, "src/main.rs"),
    `#[path=${JSON.stringify(rustPath)}] mod redaction;
fn decode(s:&str)->Vec<u8>{assert!(s.len()%2==0);(0..s.len()).step_by(2).map(|i|u8::from_str_radix(&s[i..i+2],16).unwrap()).collect()}
fn encode(s:&str)->String{s.as_bytes().iter().map(|b|format!("{b:02x}")).collect()}
fn main(){let args:Vec<_>=std::env::args().collect();let input=std::fs::read_to_string(&args[1]).unwrap();let mut output=String::new();
for line in input.lines(){let p:Vec<_>=line.splitn(4,'\\t').collect();let text=String::from_utf8(decode(p[3])).unwrap();let scope=String::from_utf8(decode(p[2])).unwrap();let clean=redaction::Redactor::new(&scope).bounded(&text,if p[1]=="notes"{6000}else{65536},p[1]=="notes");output.push_str(&format!("{}\\t{}\\t{}\\n",p[0],p[1],encode(&clean)));}std::fs::write(&args[2],output).unwrap();}
`,
  );
  const target = join(root, ".cache/failure-redaction-comparison/native-target");
  execFileSync(
    "cargo",
    ["build", "--offline", "--manifest-path", join(runner, "Cargo.toml"), "--target-dir", target],
    { stdio: "pipe", timeout: 180000 },
  );
  const input = join(work, "native-input.tsv"),
    output = join(work, "native-output.tsv");
  writeFileSync(
    input,
    records
      .map(
        (r) =>
          `${r.id}\t${r.section}\t${Buffer.from(r.scope).toString("hex")}\t${Buffer.from(r.text).toString("hex")}`,
      )
      .join("\n"),
  );
  const binary = join(
    target,
    "debug",
    process.platform === "win32" ? "diagnostic-redaction-eval.exe" : "diagnostic-redaction-eval",
  );
  execFileSync(binary, [input, output], { stdio: "pipe", timeout: 30000 });
  const results = new Map();
  for (const line of readFileSync(output, "utf8").trim().split(/\r?\n/)) {
    const [id, section, value] = line.split("\t");
    results.set(`${id}:${section}`, Buffer.from(value, "hex").toString());
  }
  assert.equal(results.size, records.length);
  return results;
}
