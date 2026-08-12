// electron-builder afterSign hook: notarize the macOS app, but ONLY when Apple
// credentials are present. Absent creds -> skip quietly so CI still emits an
// unsigned dev artifact (signing is supported, not a v1 blocker).
const { notarize } = require('@electron/notarize')

exports.default = async function notarizing(context) {
  const { electronPlatformName, appOutDir } = context
  if (electronPlatformName !== 'darwin') return

  const { APPLE_ID, APPLE_APP_SPECIFIC_PASSWORD, APPLE_TEAM_ID } = process.env
  if (!APPLE_ID || !APPLE_APP_SPECIFIC_PASSWORD || !APPLE_TEAM_ID) {
    console.log('notarize: Apple credentials absent — skipping (unsigned dev build).')
    return
  }

  const appName = context.packager.appInfo.productFilename
  console.log(`notarize: submitting ${appName}.app…`)
  await notarize({
    tool: 'notarytool',
    appPath: `${appOutDir}/${appName}.app`,
    appleId: APPLE_ID,
    appleIdPassword: APPLE_APP_SPECIFIC_PASSWORD,
    teamId: APPLE_TEAM_ID,
  })
  console.log('notarize: done.')
}
