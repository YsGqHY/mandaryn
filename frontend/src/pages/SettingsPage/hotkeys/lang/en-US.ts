import type { Messages } from '@/i18n';

// Action names live under the backend-agreed key `hotkeys.actions.<id>`
// (LabelKey convention in internal/hotkeys/registry.go), not under settings.* —
// ActionInfo.labelKey already points there, so both surfaces share one string.
export const hotkeysEnUS: Messages = {
  hotkeys: {
    actions: {
      record: {
        toggle: 'Start / stop recording',
        pause: 'Pause / resume recording',
      },
      window: {
        show: 'Show / hide main window',
      },
      playback: {
        play: 'Play / pause',
        back: 'Back 5 seconds',
        forward: 'Forward 5 seconds',
        prevSegment: 'Previous segment',
        nextSegment: 'Next segment',
        recenter: 'Recenter on current segment',
      },
      nav: {
        record: 'Go to Record',
        library: 'Go to Library',
        models: 'Go to Models',
        settings: 'Go to Settings',
      },
      search: {
        focus: 'Focus search',
      },
      role: {
        rename: 'Rename speaker role',
      },
      session: {
        delete: 'Delete session',
      },
      model: {
        switchSource: 'Switch model source (local / online)',
      },
      ux: {
        leanMode: 'Toggle lean mode',
      },
    },
  },
  settings: {
    hotkeys: {
      title: 'Keyboard shortcuts',
      hint: 'Customizable shortcuts grouped by scenario. To rebind, just press the combination — conflicts are checked before saving, and an occupied or reserved combination is reported with its reason.',
      unavailable: {
        title: 'This platform does not support system-wide shortcuts',
        body: 'This build cannot register global shortcuts at the system level (missing platform support or permission). The list stays viewable, but rebinding, disabling and resetting are unavailable.',
      },
      capture: {
        hint: 'Click "Rebind", then press the combination in the field that appears. Press Esc to cancel.',
        aria: 'Press a key combination to record the shortcut',
        placeholder: 'Press a key combination…',
        cancelHint: 'Esc to cancel',
      },
      resetAll: {
        action: 'Reset all to defaults',
        title: 'Reset all shortcuts',
        message: 'Every action returns to its default shortcut and your custom bindings are discarded. Continue?',
        ok: 'Reset',
        cancel: 'Cancel',
      },
      scope: {
        global: 'Global',
        app: 'In-app',
      },
      row: {
        unwired: 'Not wired',
        unbound: 'No combination',
        disabled: 'Disabled',
        defaultValue: 'Default {{accel}}',
        noDefault: 'Unbound by default',
        registerFailed: 'Registration failed: {{reason}}',
        capture: 'Rebind',
        cancel: 'Cancel',
        disable: 'Disable',
        reset: 'Reset',
      },
      issue: {
        register: 'That combination is already taken by the system or another app, so it was not registered (your previous binding was restored).',
        conflict: 'That combination conflicts with another action or is system-reserved and cannot be saved.',
        preview: 'Normalized preview: {{accel}}',
        invalidKey: 'That key cannot be bound: {{key}}',
        reasonSystem: 'System-reserved',
        reasonDuplicate: 'Duplicate of another action',
        conflictItem: '{{reason}}: {{action}}',
        conflictItemNoAction: '{{reason}}',
      },
      errors: {
        ERR_HOTKEY_CONFLICT: 'The shortcut conflicts with another one; nothing was saved.',
        ERR_HOTKEY_REGISTER: 'The system refused to register that combination; nothing was saved.',
        ERR_INVALID_ARGUMENT: 'The combination format is invalid. Please record it again.',
        ERR_NOT_FOUND: 'That action could not be found; it may have been removed.',
        ERR_STATE_FORBIDDEN: 'This action is not allowed in the current state.',
        ERR_DB_WRITE: 'Writing to the database failed. Please retry.',
      },
      groups: {
        recording: 'Recording',
        playback: 'Playback',
        navigation: 'Navigation',
        search: 'Search',
        editing: 'Editing',
        model: 'Models',
        ux: 'Lean UX',
      },
      lean: {
        title: 'Lean mode',
        switchLabel: 'Enable lean mode (keep core regions, hide advanced panels)',
        hint: 'The lists below are decided by the backend. Once enabled, every page chooses visibility from the same lists — no page hides something another page keeps.',
        visibleTitle: 'Kept regions',
        hiddenRegionsTitle: 'Hidden regions',
        hiddenControlsTitle: 'Hidden controls',
        regions: {
          status: 'Top status bar (recording / degraded / errors)',
          record: 'Recorder body',
          library: 'Library list',
          session: 'Session detail (playback + lyrics)',
          settingsAdvanced: 'Settings “advanced” section',
          settingsDiagnostics: 'Settings diagnostics section',
          modelsAdmin: 'Model admin details',
          libraryInsights: 'Library statistics and trends',
          diagnostics: 'Diagnostics panel',
        },
        controls: {
          statusSecondaryBadges: 'Secondary badges in the status bar',
          libraryBulkActions: 'Bulk selection and bulk delete',
          sessionExportOptions: 'Secondary export options',
          recordSourceHints: 'Technical hints in the source list',
          settingsUpdateOptions: 'Update check frequency',
          settingsDatabaseStats: 'Table row statistics',
          settingsSecurityNotes: 'Long-form security notes',
        },
      },
    },
  },
};
