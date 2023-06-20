# Configuration file for the Sphinx documentation builder.
#
# This file only contains a selection of the most common options. For a full
# list see the documentation:
# https://www.sphinx-doc.org/en/master/usage/configuration.html

# -- Path setup --------------------------------------------------------------

# If extensions (or modules to document with autodoc) are in another directory,
# add these directories to sys.path here. If the directory is relative to the
# documentation root, use os.path.abspath to make it absolute, like shown here.
#
# import os
# import sys
# sys.path.insert(0, os.path.abspath('.'))
from docutils import nodes
from os.path import isdir, isfile, join, basename, dirname
from os import makedirs, getenv
from shutil import copyfile
from subprocess import run, STDOUT

# -- Project information -----------------------------------------------------

project = 'NRI Plugins'
copyright = '2023, various'
author = 'various'

master_doc = 'docs/index'


##############################################################################
#
# This section determines the behavior of links to local items in .md files.
#
#  if built with GitHub workflows:
#
#     the GitHub URLs will use the commit SHA (GITHUB_SHA environment variable
#     is defined by GitHub workflows) to link to the specific commit.
#
##############################################################################

baseBranch = "main"
commitSHA = getenv('GITHUB_SHA')
githubServerURL = getenv('GITHUB_SERVER_URL')
githubRepository = getenv('GITHUB_REPOSITORY')
if githubServerURL and githubRepository:
    githubBaseURL = join(githubServerURL, githubRepository)
else:
    githubBaseURL = "https://github.com/containers/nri-plugins/"

githubFileURL = join(githubBaseURL, "blob/")
githubDirURL = join(githubBaseURL, "tree/")
if commitSHA:
    githubFileURL = join(githubFileURL, commitSHA)
    githubDirURL = join(githubDirURL, commitSHA)
else:
    githubFileURL = join(githubFileURL, baseBranch)
    githubDirURL = join(githubDirURL, baseBranch)

# Version displayed in the upper left corner of the site
ref = getenv('GITHUB_REF', default="")
if ref == "refs/heads/main":
    version = "devel"
elif ref.startswith("refs/heads/release-"):
    # For release branches just show the latest tag name
    buildVersion = getenv("BUILD_VERSION", default="unknown")
    version = buildVersion.split('-')[0]
elif ref.startswith("refs/tags/"):
    version = ref[len("refs/tags/"):]
else:
    version = getenv("BUILD_VERSION", default="unknown")

release = getenv("BUILD_VERSION", default="unknown")

# Versions to show in the version menu
if getenv('VERSIONS_MENU'):
    html_context = {
        'versions_menu': True,
        'versions_menu_this_version': getenv('VERSIONS_MENU_THIS_VERSION', version)}

# -- General configuration ---------------------------------------------------

# Add any Sphinx extension module names here, as strings. They can be
# extensions coming with Sphinx (named 'sphinx.ext.*') or your custom
# ones.
extensions = ['myst_parser', 'sphinx_markdown_tables']
myst_enable_extensions = ['substitution']
source_suffix = {'.rst': 'restructuredtext','.md': 'markdown'}

# Substitution variables
def module_version(module, version):
    version=version.split('-', 1)[0]
    if module == 'github.com/intel/goresctrl':
        version = '.'.join(version.split('.')[0:2]) + '.0'
    return version

def gomod_versions(modules):
    versions = {}
    gocmd = run(['go', 'list', '-m', '-f', '{{.GoVersion}}'],
                check=True, capture_output=True, universal_newlines=True)
    versions['golang'] = gocmd.stdout.strip()
    for m in modules:
        gocmd = run(['go', 'list', '-m', '-f', '{{.Version}}', '%s' % m],
                    check=True, capture_output=True, universal_newlines=True)
        versions[m] = module_version(m, gocmd.stdout.strip())
    return versions

mod_versions = gomod_versions(['github.com/intel/goresctrl'])
myst_substitutions = {
    'golang_version': mod_versions['golang'],
    'goresctrl_version': mod_versions['github.com/intel/goresctrl']
}
myst_heading_anchors = 3

myst_url_schemes = {
    "http": None,
    "https": None,
    "ftp": None,
    "mailto": None,
    "blob": githubFileURL + "{{path}}",
    "tree": githubDirURL + "{{path}}",
}

# Add any paths that contain templates here, relative to this directory.
templates_path = ['_templates']

# List of patterns, relative to source directory, that match files and
# directories to ignore when looking for source files.
# This pattern also affects html_static_path and html_extra_path.
exclude_patterns = ['_build', '.github', '_work', 'generate', 'README.md', 'TODO.md', 'SECURITY.md', 'CODE-OF-CONDUCT.md', 'docs/releases', 'test/self-hosted-runner/README.md', 'test/e2e/README.md', 'docs/resource-policy/releases', 'docs/resource-policy/README.md','test/statistics-analysis/README.md', 'docs/memtierd/README.md']

# -- Options for HTML output -------------------------------------------------

# The theme to use for HTML and HTML Help pages.  See the documentation for
# a list of builtin themes.
#
html_theme = 'sphinx_rtd_theme'

html_theme_options = {
    'display_version': True,
}
