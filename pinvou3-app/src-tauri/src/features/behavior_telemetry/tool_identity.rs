/// Identify a single foreground connector invocation from tool input. The
/// shell result belongs to the whole command, so an ambiguous command stays
/// generic. Only the returned catalog key is eligible for telemetry.
pub(super) fn connector_cli_key(command: &str) -> Option<&'static str> {
    let command = command.trim();
    let command = shell_payload(command).unwrap_or(command);
    let (command, call_operator) = match command.strip_prefix('&') {
        Some(rest) if rest.starts_with(char::is_whitespace) => (rest.trim_start(), true),
        Some(_) => return None,
        None => (command, false),
    };
    let mut quote = None;
    let mut program_end = command.len();
    let mut chars = command.char_indices().peekable();
    while let Some((index, ch)) = chars.next() {
        match (quote, ch) {
            (Some(open), close) if open == close => quote = None,
            (Some('\''), _) => {}
            (_, '\x60') => return None,
            (Some('"'), '$') if chars.peek().is_some_and(|&(_, next)| next == '(') => {
                return None;
            }
            (Some('"'), '\\')
                if chars
                    .peek()
                    .is_some_and(|&(_, next)| matches!(next, '"' | '\\' | '$')) =>
            {
                chars.next();
            }
            (Some(_), _) => {}
            (None, '\'' | '"') => quote = Some(ch),
            (None, '\\')
                if chars
                    .peek()
                    .is_some_and(|&(_, next)| matches!(next, '\n' | '\r')) =>
            {
                chars.next();
                if chars.peek().is_some_and(|&(_, next)| next == '\n') {
                    chars.next();
                }
            }
            (None, '|' | '&' | ';' | '(' | ')' | '{' | '}' | '<' | '>' | '\n' | '\r') => {
                return None;
            }
            (None, c) if c.is_whitespace() => program_end = program_end.min(index),
            _ => {}
        }
    }
    if quote.is_some() {
        return None;
    }
    let program = &command[..program_end];
    // A quoted string without a call operator can be a value-only expression.
    // Without the executed shell's dialect, that is not a connector outcome.
    if !call_operator && program.starts_with(['"', '\'']) {
        return None;
    }
    let program = program
        .strip_prefix('"')
        .and_then(|word| word.strip_suffix('"'))
        .or_else(|| {
            program
                .strip_prefix('\'')
                .and_then(|word| word.strip_suffix('\''))
        })
        .unwrap_or(program);
    if program.contains(['"', '\'', '$', '%']) {
        return None;
    }
    let executable = program.rsplit(['/', '\\']).next()?.to_ascii_lowercase();
    let name = [".exe", ".cmd", ".bat"]
        .iter()
        .find_map(|extension| executable.strip_suffix(extension))
        .unwrap_or(&executable);
    match name {
        "lark-cli" => Some("feishu"),
        "wecom-cli" => Some("wecom"),
        "dws" => Some("dingtalk"),
        "tmeet" => Some("tmeet"),
        "eip" | "eip-cli" | "eip-cli-aarch64" => Some("eip"),
        "zhidao" | "zhidao-cli" | "zhidao-cli-aarch64" => Some("zhidao"),
        _ => None,
    }
}

fn shell_payload(command: &str) -> Option<&str> {
    let (shell, rest) = command.split_once(char::is_whitespace)?;
    let (flag, argument) = rest.trim_start().split_once(char::is_whitespace)?;
    if !matches!(shell.rsplit('/').next()?, "bash" | "zsh" | "sh")
        || !matches!(flag, "-c" | "-lc" | "-cl")
    {
        return None;
    }
    let body = argument.trim().strip_prefix('\'')?.strip_suffix('\'')?;
    // Restrict unwrapping to a single literal argument with no extra shell
    // syntax around it. The command inside is checked again as a normal input.
    (!body.contains('\'')).then_some(body.trim())
}

#[cfg(test)]
mod tests {
    use super::connector_cli_key;

    #[test]
    fn recognizes_only_the_executed_connector() {
        for (command, key) in [
            (
                "lark-cli base list --params '{\"filter\":\"a|b\"}'",
                "feishu",
            ),
            (
                "& \"C:\\Program Files\\Pinvou\\wecom-cli.EXE\" list",
                "wecom",
            ),
            ("dws calendar list", "dingtalk"),
            ("tmeet schedule list", "tmeet"),
            ("/bin/bash -lc 'lark-cli base list'", "feishu"),
            ("/opt/local/bin/eip-cli-aarch64 status", "eip"),
            ("zhidao search \\\n --query demo", "zhidao"),
        ] {
            assert_eq!(connector_cli_key(command), Some(key), "{command}");
        }

        for command in [
            "echo lark-cli",
            "\"lark-cli\"",
            "wecom-cli-helper list",
            "lark-cli list || true",
            "/bin/bash -lc 'lark-cli list || true'",
            "/bin/bash -lc 'lark-cli list' ; 'true'",
            "lark-cli list | sort",
            "lark-cli list &",
            "lark-cli list\nwecom-cli list",
            "lark-cli list --value \"$(wecom-cli list)\"",
            "lark-cli list --value \x60wecom-cli\x60",
            "lark-cli list --value 'unclosed",
        ] {
            assert_eq!(connector_cli_key(command), None, "{command}");
        }
    }
}
