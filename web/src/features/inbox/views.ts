/** Sent mail has two views: all of it, and what still awaits a reply. */
export function isSentView(folder: string): boolean { return folder === 'sent' || folder === 'awaiting' }
