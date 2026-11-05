// Explicit async exit adapter for legacy CommonJS smokes. Synchronous optional
// dependency exits happen before creating resources and keep their exit-2 API.
module.exports = async code => {
  let status = code;
  try {
    const { fixtureScope } = await import('./fixture-scope.mjs');
    await fixtureScope.close();
  } catch (error) {
    console.error('Test fixture retirement failed', error);
    if (!status) status = 1;
  }
  process.exit(status);
};
