//! Windows system-tray lifecycle for the main application window.

use std::sync::atomic::{AtomicBool, Ordering};

use tauri::{
    Manager,
    menu::{Menu, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
};

use crate::platform::prefs::{Language, UserPrefs};

const TRAY_ID: &str = "pinvou-main-tray";
const SHOW_MENU_ID: &str = "tray-show-main";
const EXIT_MENU_ID: &str = "tray-exit-app";

static TRAY_READY: AtomicBool = AtomicBool::new(false);

#[derive(Debug, PartialEq, Eq)]
struct TrayLabels {
    show: &'static str,
    exit: &'static str,
}

fn labels_for(language: Language) -> TrayLabels {
    match language {
        Language::ZhHans => TrayLabels {
            show: "显示主窗口",
            exit: "退出应用",
        },
        Language::En => TrayLabels {
            show: "Show main window",
            exit: "Quit",
        },
        Language::Ja => TrayLabels {
            show: "メインウィンドウを表示",
            exit: "終了",
        },
    }
}

fn activate_main_window(app: &tauri::AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        crate::platform::window_startup::activate_main_window(window);
    }
}

/// Install one process-wide tray icon. A failed installation leaves normal
/// window closing intact so the application can never become hidden without a
/// recovery entry point.
pub(crate) fn install(app: &tauri::App) -> tauri::Result<()> {
    let labels = labels_for(UserPrefs::load().language);
    let show = MenuItem::with_id(app, SHOW_MENU_ID, labels.show, true, None::<&str>)?;
    let exit = MenuItem::with_id(app, EXIT_MENU_ID, labels.exit, true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &exit])?;

    let mut builder = TrayIconBuilder::with_id(TRAY_ID)
        .tooltip("Pinvou")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id().as_ref() {
            SHOW_MENU_ID => activate_main_window(app),
            EXIT_MENU_ID => app.exit(0),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if matches!(
                event,
                TrayIconEvent::Click {
                    button: MouseButton::Left,
                    button_state: MouseButtonState::Up,
                    ..
                }
            ) {
                activate_main_window(tray.app_handle());
            }
        });

    if let Some(icon) = app.default_window_icon() {
        builder = builder.icon(icon.clone());
    }
    builder.build(app)?;
    TRAY_READY.store(true, Ordering::Release);
    Ok(())
}

fn should_hide_main_window(label: &str, tray_ready: bool) -> bool {
    label == "main" && tray_ready
}

/// Handle only the Windows main-window close request. Auxiliary windows keep
/// their normal close semantics, while an explicit tray "Quit" exits the app.
pub(crate) fn hide_main_on_close(window: &tauri::Window, event: &tauri::WindowEvent) -> bool {
    let tauri::WindowEvent::CloseRequested { api, .. } = event else {
        return false;
    };
    if !should_hide_main_window(window.label(), TRAY_READY.load(Ordering::Acquire)) {
        return false;
    }

    api.prevent_close();
    if let Err(error) = window.hide() {
        eprintln!("[pinvou3-app] failed to hide main window to tray: {error}");
    }
    true
}

#[cfg(test)]
mod tests {
    use super::{TrayLabels, labels_for, should_hide_main_window};
    use crate::platform::prefs::Language;

    #[test]
    fn close_to_tray_applies_only_to_main_window_after_tray_is_ready() {
        assert!(should_hide_main_window("main", true));
        assert!(!should_hide_main_window("main", false));
        assert!(!should_hide_main_window("pet", true));
    }

    #[test]
    fn tray_menu_copy_covers_all_supported_languages() {
        assert_eq!(
            labels_for(Language::ZhHans),
            TrayLabels {
                show: "显示主窗口",
                exit: "退出应用"
            }
        );
        assert_eq!(
            labels_for(Language::En),
            TrayLabels {
                show: "Show main window",
                exit: "Quit"
            }
        );
        assert_eq!(
            labels_for(Language::Ja),
            TrayLabels {
                show: "メインウィンドウを表示",
                exit: "終了"
            }
        );
    }
}
