//! `/agent` command.

use crate::commands::traits::{CommandInfo, RegisterCommand};
use crate::localization::MessageId;
use crate::tui::app::{App, AppAction};

use super::CommandResult;

pub(in crate::commands) const COMMAND_INFO: CommandInfo = CommandInfo {
    name: "agent",
    aliases: &["daili"],
    usage: "/agent [N] <task>",
    description_id: MessageId::CmdAgentDescription,
};

pub(in crate::commands) struct AgentCmd;

impl RegisterCommand for AgentCmd {
    fn info() -> &'static CommandInfo {
        &COMMAND_INFO
    }

    fn execute(app: &mut App, arg: Option<&str>) -> CommandResult {
        agent(app, arg)
    }
}

pub fn agent(_app: &mut App, arg: Option<&str>) -> CommandResult {
    if let Some(action) = parse_agent_control_action(arg) {
        if action.action == "cancel" {
            return CommandResult::with_message_and_action(
                format!("Cancelling agent {}...", action.agent_id),
                AppAction::CancelSubAgent {
                    agent_id: action.agent_id,
                },
            );
        }
        let message = format!(
            "Call `agent` with action `{}`, agent_id `{}`, then summarize the returned status for the user. Do not start a new agent.",
            action.action, action.agent_id
        );
        return CommandResult::with_message_and_action(
            format!("Agent {} requested for {}.", action.action, action.agent_id),
            AppAction::SendMessage(message),
        );
    }

    let (max_depth, task) = match super::util::parse_depth_prefixed_arg(arg, 1) {
        Ok(parsed) => parsed,
        Err(message) => return CommandResult::error(message),
    };
    let task = match task {
        Some(task) if !task.trim().is_empty() => task.trim().to_string(),
        _ => {
            return CommandResult::error(
                "Usage: /agent [N] <task>\n\n\
                 Opens a persistent sub-agent session with recursive agent depth N (0-3, default 1).",
            );
        }
    };
    let message = format!(
        "Launch one sub-agent for this task by calling `agent` with name `slash_agent`, `prompt: {task:?}`, and `max_depth: {max_depth}`. Use `handle_read` on a sub-agent transcript handle if you need more detail (verbose spawn receipts and scoped status rows carry one); {handle_read_hint}. Verify any claimed side effects before reporting success.",
        handle_read_hint = crate::tools::subagent::HANDLE_READ_ACTIVATION_HINT
    );
    CommandResult::with_message_and_action(
        format!("Opening persistent sub-agent at depth {max_depth}..."),
        AppAction::SendMessage(message),
    )
}

struct AgentControlAction {
    action: &'static str,
    agent_id: String,
}

fn parse_agent_control_action(arg: Option<&str>) -> Option<AgentControlAction> {
    let arg = arg?.trim();
    let (action, rest) = arg.split_once(char::is_whitespace)?;
    let action = match action {
        "status" | "inspect" => "status",
        "peek" | "progress" => "peek",
        "cancel" | "stop" | "abort" => "cancel",
        _ => return None,
    };
    let agent_id = rest.trim();
    if agent_id.is_empty() || agent_id.contains(char::is_whitespace) {
        return None;
    }
    Some(AgentControlAction {
        action,
        agent_id: agent_id.to_string(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    use crate::tui::app::TuiOptions;

    fn test_app() -> App {
        let options = TuiOptions {
            ..crate::test_support::test_tui_options(PathBuf::from("."))
        };
        App::new(options, &crate::config::Config::default())
    }

    #[test]
    fn agent_control_actions_route_to_existing_agent_tool() {
        let mut app = test_app();
        let result = agent(&mut app, Some("peek agent_123"));

        assert!(!result.is_error);
        let Some(AppAction::SendMessage(message)) = result.action else {
            panic!("expected SendMessage action");
        };
        assert!(message.contains("action `peek`"));
        assert!(message.contains("agent_id `agent_123`"));
        assert!(message.contains("Do not start a new agent"));

        let result = agent(&mut app, Some("cancel agent_123"));
        let Some(AppAction::CancelSubAgent { agent_id }) = result.action else {
            panic!("expected CancelSubAgent action");
        };
        assert_eq!(agent_id, "agent_123");
    }

    #[test]
    fn forkguard_slash_agent_dispatch_teaches_handle_read_activation() {
        // `handle_read` is deferred on stock hosts, so the dispatch brief
        // must teach the `tool_search` activation path instead of pointing
        // the model at a tool absent from its first-turn catalog (Pinvou
        // #490 phantom-tool class).
        let mut app = test_app();
        let result = agent(&mut app, Some("inspect the failing test"));
        let Some(AppAction::SendMessage(message)) = result.action else {
            panic!("expected SendMessage action");
        };
        assert!(message.contains("`handle_read`"));
        assert!(
            message.contains("activate it via `tool_search` first"),
            "the dispatch brief must teach the handle_read activation path:\n{message}"
        );
        assert!(
            message.contains("try calling `handle_read` directly"),
            "allowlist-filtered hosts can keep handle_read in the catalog while \
             removing tool_search; the brief must try the direct call before \
             declaring transcript reads unavailable:\n{message}"
        );
        assert!(
            message.contains("transcript reads are unavailable in this session"),
            "when tool_search cannot surface handle_read and the direct call \
             errors, the brief must degrade honestly instead of dead-ending or \
             over-promising:\n{message}"
        );
        assert!(
            !message.contains("hydrate when called by name"),
            "registered deferred tools do not promise a hydration mechanism; \
             the false mechanism assertion must stay gone:\n{message}"
        );
        assert!(
            !message.contains("the returned transcript_handle"),
            "the default spawn receipt strips the handle before the model \
             sees it; naming it as returned is the phantom-value class the \
             context summarizer fix removes:\n{message}"
        );
    }
}
