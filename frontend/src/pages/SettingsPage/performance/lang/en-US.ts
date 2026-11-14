import type { Messages } from '@/i18n';

export const performanceEnUS: Messages = {
  settings: {
    performance: {
      title: 'Performance budget',
      hint: 'Set the share of the whole machine this app may use. The budget only constrains inference (transcription / diarization); recording and writing to disk are never interrupted.',
      cap: {
        label: 'Machine-wide budget N',
        value: '{{value}}%',
        aria: 'Machine-wide performance budget',
        hint: 'Range 10–100 in steps of 5. Release the slider to save.',
      },
      formula: {
        title: 'This is a machine-wide share, not a cap on this app',
        other: 'Other processes: otherPct = total − self',
        headroom: 'Headroom for this app: headroom = N − otherPct',
        note: 'In other words: the busier the rest of the machine, the less is left for this app. Raising N gives this app more inference share — but the machine is still never saturated by this app alone.',
      },
      readout: {
        title: 'Live readout',
        levelValue: 'Level {{level}}',
        levelUnknown: 'Level —',
        sampledAt: 'Last sample {{time}}',
        waiting: 'Waiting for the first sample…',
      },
      levels: {
        L0: 'L0 full speed: budget is ample, inference runs at the top tier.',
        L1: 'L1 slight downgrade: the machine is getting busy, inference lowers threads / batch.',
        L2: 'L2 moderate downgrade: inference tightens further; transcription slows down but continues.',
        L3: 'L3 heavy downgrade: only minimal inference is kept so other apps stay smooth.',
        P: 'P inference paused: the budget is exhausted; inference resumes automatically once headroom returns.',
        unknown: 'The level is reported by the backend; unknown values are shown as-is.',
      },
      metrics: {
        total: 'Machine total',
        self: 'This app',
        other: 'Other processes',
        headroom: 'Headroom',
      },
      paused: {
        title: 'Inference paused (level P)',
        body: 'Recording and audio persistence are unaffected — capture and writing to disk continue. Transcription tasks queue up and resume automatically once headroom returns.',
      },
      feedback: {
        saving: 'Saving budget…',
        saved: 'Budget applied',
      },
      errors: {
        ERR_PERF_CAP_RANGE: 'Invalid budget: only values between 10 and 100 are accepted, in steps of 5.',
        ERR_DB_WRITE: 'Writing the budget to the database failed. Please retry.',
        ERR_INVALID_ARGUMENT: 'The request arguments were invalid. Please retry.',
      },
    },
  },
};
