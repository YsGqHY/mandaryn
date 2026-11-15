import type { Messages } from '@/i18n';

export const aboutEnUS: Messages = {
  settings: {
    about: {
      title: 'About & Updates',
      hint: 'See the current version and check for a newer one. Checking requires network access; the app never downloads or installs updates automatically.',
      version: {
        label: 'Current version',
        loading: 'Reading…',
        unknown: 'Unknown',
      },
      check: {
        button: 'Check for updates',
        upToDate: 'You are on the latest version.',
        available: 'A new version is available: {{version}}',
        openDownload: 'Open release page',
        manualNote: 'When a newer version exists, download and run the installer yourself — this app does not self-update silently.',
        offline: 'Offline mode is on, so no network request was made. Turn it off and retry.',
        failed: 'Update check failed: {{message}}',
        errors: {
          ERR_DOWNLOAD_NETWORK: 'Could not reach the version manifest (network unreachable, or the manifest is not publicly readable for this repository).',
          ERR_OFFLINE_ENFORCED: 'Offline mode is on, so no network request was made.',
          ERR_INVALID_ARGUMENT: 'The version manifest URL is not usable; check the configuration.',
        },
      },
      policy: {
        title: 'Why there is no auto-update',
        body: 'Replacing executables inside the install directory requires elevation and bypasses software management; this app only notifies you, and you install the update manually.',
      },
    },
  },
};
